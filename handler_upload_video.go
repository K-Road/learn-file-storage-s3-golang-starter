package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxUploadSize = 1 << 30

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	err := r.ParseMultipartForm(maxUploadSize)
	if err != nil {
		http.Error(w, "File is too large. Maximum size is 1 GB.", http.StatusRequestEntityTooLarge)
		return
	}

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
		respondWithError(w, http.StatusBadRequest, "Unable to get video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse from file", err)
		return
	}
	defer file.Close()

	fmt.Println("uploading video", videoID, "by user", userID)

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", err)
		return
	}
	tmp, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create file", err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	_, err = io.Copy(tmp, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to write file", err)
		return
	}

	tmp.Seek(0, io.SeekStart)

	//get aspect ratio
	aspectRatio, err := getVideoAspectRatio(tmp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to find aspect", err)
		return
	}

	aspect := ""
	switch aspectRatio {
	case "16:9":
		aspect = "landscape"
	case "9:16":
		aspect = "portrait"
	default:
		aspect = "other"
	}
	key := getAssetPath(mediaType)
	key = filepath.Join(aspect, key)

	processedFilePath, err := processVideoForFastStart(tmp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable fast process", err)
		return
	}
	defer os.Remove(processedFilePath)

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open processed file", err)
		return
	}
	defer processedFile.Close()

	_, err = cfg.s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload", err)
		return
	}

	videoURL := cfg.getObjectURL(key)
	video.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	type VideoStream struct {
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Duration  string `json:"duration"`
	}
	type FFprobeResult struct {
		Streams []VideoStream `json:"streams"`
	}

	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", err
	}

	var result FFprobeResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return "", fmt.Errorf("could not parse ffprobe: %v", err)
	}

	if len(result.Streams) == 0 {
		return "", errors.New("no video streams found")
	}

	stream := result.Streams[0]
	aspectRatio := float64(stream.Height) / float64(stream.Width)
	const tolerance = 0.01
	switch {
	case almostEqual(aspectRatio, 9.0/16.0, tolerance):
		return "16:9", nil
	case almostEqual(aspectRatio, 4.0/3.0, tolerance):
		return "4:3", nil
	case almostEqual(aspectRatio, 16.0/9.0, tolerance):
		return "9:16", nil
	case almostEqual(aspectRatio, 1.0, tolerance):
		return "1:1", nil
	default:
		return "Other", nil
	}
}

func almostEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) < tolerance
}

func processVideoForFastStart(filePath string) (string, error) {
	processedFilePath := fmt.Sprintf("%s.processing", filePath)
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", processedFilePath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(processedFilePath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return processedFilePath, nil

}
