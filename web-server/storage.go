package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var s3Client *s3.Client

func initStorage() {
	if CloudStoreEnabled {
		cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(AwsRegionName))
		if err != nil {
			log.Fatalf("unable to load SDK config: %v", err)
		}
		s3Client = s3.NewFromConfig(cfg)
		log.Println("Cloud storage (S3) enabled")
	} else {
		log.Println("Local storage enabled")
		ensureDataDir()
	}
}

func toS3Key(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

func saveFile(p string, data []byte) error {
	if CloudStoreEnabled {
		key := toS3Key(p)
		_, err := s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: aws.String(S3BucketName),
			Key:    aws.String(key),
			Body:   bytes.NewReader(data),
		})
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
}

func readFile(p string) ([]byte, error) {
	if CloudStoreEnabled {
		key := toS3Key(p)
		out, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
			Bucket: aws.String(S3BucketName),
			Key:    aws.String(key),
		})
		if err != nil {
			return nil, err
		}
		defer out.Body.Close()
		return io.ReadAll(out.Body)
	}
	return os.ReadFile(p)
}

func listDirs(prefix string) ([]string, error) {
	if CloudStoreEnabled {
		keyPrefix := toS3Key(prefix)
		if keyPrefix != "" && !strings.HasSuffix(keyPrefix, "/") {
			keyPrefix += "/"
		}
		
		var dirs []string
		paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
			Bucket:    aws.String(S3BucketName),
			Prefix:    aws.String(keyPrefix),
			Delimiter: aws.String("/"),
		})

		for paginator.HasMorePages() {
			page, err := paginator.NextPage(context.TODO())
			if err != nil {
				return nil, err
			}
			for _, cp := range page.CommonPrefixes {
				dir := strings.TrimPrefix(*cp.Prefix, keyPrefix)
				dir = strings.TrimSuffix(dir, "/")
				dirs = append(dirs, dir)
			}
		}
		return dirs, nil
	}

	entries, err := os.ReadDir(prefix)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs, nil
}

func listFiles(prefix string) ([]string, error) {
	if CloudStoreEnabled {
		keyPrefix := toS3Key(prefix)
		if keyPrefix != "" && !strings.HasSuffix(keyPrefix, "/") {
			keyPrefix += "/"
		}
		
		var files []string
		paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
			Bucket:    aws.String(S3BucketName),
			Prefix:    aws.String(keyPrefix),
			Delimiter: aws.String("/"),
		})

		for paginator.HasMorePages() {
			page, err := paginator.NextPage(context.TODO())
			if err != nil {
				return nil, err
			}
			for _, obj := range page.Contents {
				name := strings.TrimPrefix(*obj.Key, keyPrefix)
				if name != "" && !strings.Contains(name, "/") {
					files = append(files, name)
				}
			}
		}
		return files, nil
	}

	entries, err := os.ReadDir(prefix)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	return files, nil
}

func deleteFile(p string) error {
	if CloudStoreEnabled {
		key := toS3Key(p)
		_, err := s3Client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
			Bucket: aws.String(S3BucketName),
			Key:    aws.String(key),
		})
		return err
	}
	return os.Remove(p)
}
