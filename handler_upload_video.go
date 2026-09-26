package main

import (
	"bytes"
	"errors"
	"fmt"
	"encoding/json"
	"io"
	"mime"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/Yishen1011/tubely/internal/auth"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Setting upload limit
	const maxUpload = 1 << 30
	// func MaxBytesReader(w ResponseWriter, r io.ReadCloser, n int64) io.ReadCloser
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)

	// Extract videoID from URL path parameters
	videoIDString := r.PathValue("videoID")
	// Parse UUID to a string
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Authenticate user with token
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

	// Get video metadata from database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error retrieving video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized access to video", nil)
		return
	}

	// Parse the uploaded video file from the form data
	const maxMemory = 10 << 20
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
    	respondWithError(w, http.StatusBadRequest, "Unable to parse multipart form", err)
		return
	}
	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close() 

	// Validate the uploaded video file as .mp4
	mediaType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	// Upload file to a temporary file on disk
	// func CreateTemp(dir, pattern string) (*os.File, error)
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temp file", err)
		return
	}
	// tempFile.Name() returns the name of the file as presented to Open.
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	if _, err = io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying file", err)
		return
	}

	if _, err = tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error resetting tmp file pointer", err)
		return
	}

	// Check for aspect ratio to add prefix onto key based on video aspect ratio
	directory := ""
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to obtain aspect ratio of video", err)
		return
	}
	switch aspectRatio {
	case "16:9":
		directory = "landscape"
	case "9:16":
		directory = "portrait"
	default:
		directory = "other"
	}

	key := path.Join(directory, getAssetPath(mediaType))

	// Function to help fast start for video
	// Only GET request 1 instead of 3
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to process video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath)

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open fast video temp file", err)
		return
	}
	defer processedFile.Close()

	// *os.File which allows to be Read by s3.PutObjectInput.Body
	// aws.String inputs string and outputs *string
	// func (c *Client) PutObject(ctx context.Context, params *PutObjectInput, optFns ...func(*Options)) (*PutObjectOutput, error)
	if _, err := cfg.s3Client.PutObject(r.Context(), 
		&s3.PutObjectInput{
			Bucket:      aws.String(cfg.s3Bucket),
			Key:         aws.String(key),
			Body:        processedFile,
			ContentType: aws.String(mediaType),
		},
	); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to upload video to s3 bucket", err)
		return
	}

	// Update VideoURL video metadata in database
	url := cfg.getVideoURL(cfg.s3Bucket, cfg.s3Region, key)
	video.VideoURL = &url

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error)  {
	cmd := exec.Command(
		"ffprobe", 
		"-v", "error", 
		"-print_format", "json", 
		"-show_streams", filePath,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", err
	}

	var output struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return "", err
	}
	if len(output.Streams) == 0 {
		return "", errors.New("There are no streams on the video")
	}

	return CalculateAspectRatio(output.Streams[0].Width, output.Streams[0].Height), nil
}

func CalculateAspectRatio(width, height int) string {
	expected16by9 := 16.0 / 9.0
	expected9by16 := 9.0 / 16.0
	tolerance := 0.01 

	actualRatio := float64(width) / float64(height)

	difference16by9 := actualRatio - expected16by9
    difference9by16 := actualRatio - expected9by16

	if math.Abs(difference16by9) < tolerance {
		return "16:9"
	} else if math.Abs(difference9by16) < tolerance {
		return "9:16"
	}

	return "other"
}

func processVideoForFastStart(inputFilePath string) (string, error) {
	processedFilePath := fmt.Sprintf("%s.processing", inputFilePath)
	
	cmd := exec.Command(
		"ffmpeg",
		"-i", inputFilePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4", 
		processedFilePath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", err
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
