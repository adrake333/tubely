package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)




type ffprobeOutput struct {
	Streams		[]Stream	`json:"streams"`
}

type Stream struct {
	Width		int		`json:"width"`
	Height		int		`json:"height"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var ffprobeOut ffprobeOutput
	err = json.Unmarshal(out.Bytes(), &ffprobeOut)
	if err != nil {
		return "", err
	}

	if len(ffprobeOut.Streams) == 0 {
		return "", errors.New("empty stream")
	}
	width := ffprobeOut.Streams[0].Width
	height := ffprobeOut.Streams[0].Height
	if width <= 0 || height <= 0 {
		return "", errors.New("invalid video dimensions")
	}

	if height > width {
		ratio := float64(width) / float64(height)
		if math.Abs(ratio - 0.5625) <= 0.01 {
			return "9:16", nil
		}
	}

	if height < width {
		ratio := float64(width) / float64(height)
		if math.Abs(ratio - 1.7777) <= 0.01 {
			return "16:9", nil
		}
	}
	
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	return outputPath, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxUploadSize = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to retrieve video", err)
		return
	}
	if userID != video.UserID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", errors.New("you do not own this video"))
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	parsedType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to retrieve media type", err)
		return
	}

	if parsedType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Improper media type for video", errors.New("improper media type for video"))
		return
	}

	temp, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temp file", err)
		return
	}
	
	defer os.Remove(temp.Name())

	_, err = io.Copy(temp, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save video to temp file", err)
		return
	}

	temp.Close()

	processed, err := processVideoForFastStart(temp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video for fast start", err)
		return
	}
	
	processedFile, err := os.Open(processed)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to open fast start file", err)
		return
	}

	defer processedFile.Close()
	defer os.Remove(processed)

	aspect, err := getVideoAspectRatio(temp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to calculate aspect ratio", err)
		return
	}

	var ratio string
	if aspect == "16:9" {
		ratio = "landscape/"
	}
	if aspect == "9:16" {
		ratio = "portrait/"
	}
	if aspect == "other" {
		ratio = "other/"
	}
	
	randKey := make([]byte, 32)
	rand.Read(randKey)
	randKeyString := hex.EncodeToString(randKey)
	extensions, err := mime.ExtensionsByType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to retrieve file extensions", err)
		return
	}
	key := ratio + randKeyString + extensions[0]
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:		aws.String(cfg.s3Bucket),
		Key:		aws.String(key),
		Body:		processedFile,
		ContentType:	aws.String(parsedType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload video", err)
		return
	}

	url := fmt.Sprintf("%s,%s", cfg.s3Bucket, key)
	video.VideoURL = &url
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video", err)
		return
	}

	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to sign videoURL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signedVideo)
}
