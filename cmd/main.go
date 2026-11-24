package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"ffmpeg-api/api"
	"ffmpeg-api/internal"
)

const (
	statusTrailer = "X-Ffmpeg-Status"
	bytesTrailer  = "X-Ffmpeg-Bytes"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/process", handleProcess)
	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Println("server listening on :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("server failed:", err)
	}
}

func handleProcess(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		internal.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	defer r.Body.Close()

	req, err := decodeRequest(r.Body)
	if err != nil {
		log.Println("invalid request:", err)
		internal.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRequest(req); err != nil {
		log.Println("invalid request:", err)
		internal.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	jobPath, cleanup, err := createJobPath()
	if err != nil {
		log.Println("job directory creation failed:", err)
		internal.WriteError(w, http.StatusInternalServerError, "failed to create job directory")
		return
	}
	defer cleanup()
	log.Println("job started:", jobPath)

	var s3Client *s3.Client
	if requestUsesS3(req) {
		s3Client = internal.GetS3Client(&req.S3Config)
	}

	if err := fetchInputs(r.Context(), s3Client, req.Inputs, jobPath); err != nil {
		log.Println("input fetch failed:", err)
		internal.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Output.Stream {
		if err := runFFmpegStreaming(r.Context(), w, req, jobPath); err != nil {
			log.Println("streaming ffmpeg execution failed:", err)
			return
		}
		log.Printf("job finished in %s (streamed)", time.Since(start))
		return
	}

	if stderr, err := runFFmpeg(r.Context(), req.Commands, jobPath); err != nil {
		logFFmpegError(stderr)
		internal.WriteConsoleError(w, http.StatusInternalServerError, err.Error(), stderr)
		return
	}

	if req.Output.InlineContentType != "" {
		if err := serveFirstResult(w, r, jobPath, req); err != nil {
			log.Println("inline result failed:", err)
			internal.WriteError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	results, err := collectResults(r.Context(), s3Client, req, jobPath)
	if err != nil {
		log.Println("result collection failed:", err)
		internal.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("job finished in %s (%d outputs)", time.Since(start), len(results))
	internal.WriteJSON(w, http.StatusOK, api.ProcessResponse{Results: results})
}

func decodeRequest(body io.Reader) (api.ProcessRequest, error) {
	var req api.ProcessRequest
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return req, fmt.Errorf("request body must contain one JSON object")
		}
		return req, err
	}
	return req, nil
}

func validateRequest(req api.ProcessRequest) error {
	if len(req.Commands) == 0 {
		return fmt.Errorf("at least one command is required")
	}
	for i, command := range req.Commands {
		if len(command) == 0 {
			return fmt.Errorf("command %d is empty", i+1)
		}
	}
	for name, input := range req.Inputs {
		if name == "" || name == "." || filepath.Base(name) != name {
			return fmt.Errorf("input name %q must be a filename without a path", name)
		}
		sources := 0
		for _, present := range []bool{input.S3 != "", input.HTTP != "", input.Base64 != "", input.Temporary} {
			if present {
				sources++
			}
		}
		if sources != 1 {
			return fmt.Errorf("input %q must define exactly one source", name)
		}
	}
	if req.Output.InlineContentType != "" {
		if _, _, err := mime.ParseMediaType(req.Output.InlineContentType); err != nil {
			return fmt.Errorf("invalid inlineContentType: %w", err)
		}
		if req.Output.S3 != "" || req.Output.Base64 {
			return fmt.Errorf("inline output cannot be combined with s3 or base64 output")
		}
	}
	if req.Output.Stream {
		if req.Output.InlineContentType == "" {
			return fmt.Errorf("stream output requires inlineContentType")
		}
		last := req.Commands[len(req.Commands)-1]
		if last[len(last)-1] != "pipe:1" && last[len(last)-1] != "-" {
			return fmt.Errorf("the final streaming command must write to pipe:1")
		}
	}
	if req.Output.S3 == "" && !req.Output.Base64 && req.Output.InlineContentType == "" {
		return fmt.Errorf("at least one output option is required")
	}
	if requestUsesS3(req) {
		if req.S3Config.Endpoint == "" || req.S3Config.Region == "" || req.S3Config.AccessKey == "" || req.S3Config.SecretKey == "" {
			return fmt.Errorf("s3Config endpoint, region, accessKey, and secretKey are required for S3 transfers")
		}
	}
	return nil
}

func requestUsesS3(req api.ProcessRequest) bool {
	if req.Output.S3 != "" {
		return true
	}
	for _, input := range req.Inputs {
		if input.S3 != "" {
			return true
		}
	}
	return false
}

func createJobPath() (string, func(), error) {
	tempRoot := os.TempDir()
	if err := os.MkdirAll(tempRoot, 0700); err != nil {
		return "", nil, err
	}
	path, err := os.MkdirTemp(tempRoot, "ffmpeg-api-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		log.Println("cleaning job dir:", path)
		if err := os.RemoveAll(path); err != nil {
			log.Println("job directory cleanup failed:", err)
		}
	}
	return path, cleanup, nil
}

func fetchInputs(ctx context.Context, s3Client *s3.Client, inputs map[string]api.Input, jobPath string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, len(inputs))
	for name, input := range inputs {
		if input.Temporary {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Println("fetching input:", name)
			dst := filepath.Join(jobPath, name)
			if err := fetchInput(ctx, s3Client, input, dst); err != nil {
				errCh <- fmt.Errorf("input %s: %w", name, err)
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return nil
}

func fetchInput(ctx context.Context, s3Client *s3.Client, input api.Input, dst string) error {
	switch {
	case input.S3 != "":
		return internal.DownloadFromS3(ctx, s3Client, input.S3, dst)
	case input.HTTP != "":
		return internal.DownloadFromHTTP(ctx, input.HTTP, dst)
	case input.Base64 != "":
		return internal.WriteBase64ToFile(input.Base64, dst)
	default:
		return fmt.Errorf("input source missing")
	}
}

func runFFmpeg(ctx context.Context, commands [][]string, dir string) ([]string, error) {
	for i, args := range commands {
		start := time.Now()
		log.Printf("ffmpeg step %d: %s", i+1, strings.Join(sanitizeArgs(args), " "))
		stderr, err := executeFFmpeg(ctx, args, dir, nil)
		if err != nil {
			return stderr, err
		}
		log.Printf("ffmpeg step %d finished in %s", i+1, time.Since(start))
	}
	return nil, nil
}

func executeFFmpeg(ctx context.Context, args []string, dir string, stdout io.Writer) ([]string, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", sanitizeArgs(args)...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return consoleLines(stderr.String()), err
}

func runFFmpegStreaming(ctx context.Context, w http.ResponseWriter, req api.ProcessRequest, dir string) error {
	head := req.Commands[:len(req.Commands)-1]
	last := req.Commands[len(req.Commands)-1]
	if stderr, err := runFFmpeg(ctx, head, dir); err != nil {
		logFFmpegError(stderr)
		internal.WriteConsoleError(w, http.StatusInternalServerError, err.Error(), stderr)
		return err
	}

	w.Header().Set("Trailer", statusTrailer+", "+bytesTrailer)
	w.Header().Set("Content-Type", req.Output.InlineContentType)
	w.WriteHeader(http.StatusOK)
	stream := &streamWriter{writer: w}
	if flusher, ok := w.(http.Flusher); ok {
		stream.flusher = flusher
	}
	step := len(req.Commands)
	start := time.Now()
	log.Printf("ffmpeg step %d: %s", step, strings.Join(sanitizeArgs(last), " "))
	stderr, err := executeFFmpeg(ctx, last, dir, stream)
	w.Header().Set(bytesTrailer, strconv.FormatInt(stream.written, 10))
	if err != nil {
		w.Header().Set(statusTrailer, "error")
		logFFmpegError(stderr)
		return fmt.Errorf("ffmpeg failed after %d bytes: %w", stream.written, err)
	}
	w.Header().Set(statusTrailer, "ok")
	log.Printf("ffmpeg step %d finished in %s (%d bytes streamed)", step, time.Since(start), stream.written)
	return nil
}

type streamWriter struct {
	writer  io.Writer
	flusher http.Flusher
	written int64
}

func (w *streamWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.written += int64(n)
	if n > 0 && w.flusher != nil {
		w.flusher.Flush()
	}
	return n, err
}

func consoleLines(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return strings.Split(value, "\n")
}

func logFFmpegError(stderr []string) {
	log.Println("ffmpeg execution failed")
	for _, line := range stderr {
		log.Println("ffmpeg:", line)
	}
}

func sanitizeArgs(args []string) []string {
	out := make([]string, 1, len(args)+1)
	out[0] = "-hide_banner"
	for _, arg := range args {
		if arg != "-hide_banner" {
			out = append(out, arg)
		}
	}
	return out
}

func collectResults(ctx context.Context, s3Client *s3.Client, req api.ProcessRequest, jobPath string) (map[string]api.Result, error) {
	files, err := outputFiles(jobPath, req.Inputs)
	if err != nil {
		return nil, err
	}
	results := make(map[string]api.Result, len(files))
	for _, file := range files {
		log.Println("collecting output:", file.Name())
		path := filepath.Join(jobPath, file.Name())
		result := api.Result{}
		if req.Output.S3 != "" {
			url, err := internal.UploadToS3(ctx, s3Client, req.Output.S3, path, file.Name())
			if err != nil {
				return nil, err
			}
			result.URL = url
		}
		if req.Output.Base64 {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			result.Base64 = base64.StdEncoding.EncodeToString(data)
		}
		results[file.Name()] = result
	}
	return results, nil
}

func outputFiles(jobPath string, inputs map[string]api.Input) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(jobPath)
	if err != nil {
		return nil, err
	}
	files := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, isInput := inputs[entry.Name()]; isInput {
			continue
		}
		files = append(files, entry)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no output files")
	}
	return files, nil
}

func serveFirstResult(w http.ResponseWriter, r *http.Request, jobPath string, req api.ProcessRequest) error {
	files, err := outputFiles(jobPath, req.Inputs)
	if err != nil {
		return err
	}
	file := files[0]
	log.Println("serving inline output:", file.Name())
	w.Header().Set("Content-Type", req.Output.InlineContentType)
	http.ServeFile(w, r, filepath.Join(jobPath, file.Name()))
	return nil
}
