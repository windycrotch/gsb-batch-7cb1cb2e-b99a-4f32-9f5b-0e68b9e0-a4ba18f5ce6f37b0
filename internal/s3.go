package internal

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"ffmpeg-api/api"
)

func GetS3Client(authentication *api.S3Config) *s3.Client {
	scheme := "http"
	if authentication.UseSSL == nil || *authentication.UseSSL {
		scheme = "https"
	}
	return s3.New(s3.Options{
		Region:       authentication.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(authentication.AccessKey, authentication.SecretKey, ""),
		BaseEndpoint: aws.String(fmt.Sprintf("%s://%s", scheme, authentication.Endpoint)),
		UsePathStyle: true,
	})
}

func DownloadFromS3(ctx context.Context, client *s3.Client, s3url, dest string) error {
	bucket, key, err := parseS3(s3url)
	if err != nil {
		return err
	}

	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer out.Body.Close()

	file, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, out.Body)
	return err
}

func UploadToS3(ctx context.Context, client *s3.Client, baseURL, path, name string) (string, error) {
	bucket, prefix, err := parseS3(baseURL)
	if err != nil {
		return "", err
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	key := name
	if prefix != "" {
		key = strings.Trim(prefix, "/") + "/" + name
	}

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   file,
	})
	if err != nil {
		return "", err
	}

	return "s3://" + bucket + "/" + key, nil
}

func parseS3(value string) (string, string, error) {
	if !strings.HasPrefix(value, "s3://") {
		return "", "", fmt.Errorf("invalid S3 URL %q: expected s3://bucket/key", value)
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "s3://"), "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", fmt.Errorf("invalid S3 URL %q: expected s3://bucket/key", value)
	}
	return parts[0], strings.TrimPrefix(parts[1], "/"), nil
}
