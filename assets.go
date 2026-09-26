package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (cfg apiConfig) ensureAssetsDir() error {
	if _, err := os.Stat(cfg.assetsRoot); os.IsNotExist(err) {
		return os.Mkdir(cfg.assetsRoot, 0755)
	}
	return nil
}

func getAssetPath(mediaType string) string {
	key := make([]byte, 32)
	rand.Read(key)
	encoder := base64.RawURLEncoding.EncodeToString(key)

	extension := mediaTypeToExt(mediaType)
	return fmt.Sprintf("%s.%s", encoder, extension)
}

func (cfg apiConfig) getAssetDiskPath(assetPath string) string {
	return filepath.Join(cfg.assetsRoot, assetPath)
}

func (cfg apiConfig) getAssetURL(assetPath string) string {
	return fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, assetPath)
}

func (cfg apiConfig) getVideoURL(bucket, region, key string) string {
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", bucket, region, key)
}

func mediaTypeToExt(mediaType string) string {
	parts := strings.Split(mediaType, "/")
	if len(parts) != 2 {
		return "bin"
	}
	return parts[1]
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
