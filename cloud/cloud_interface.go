package cloud

import (
	"context"
)

// ContextKey is the type of the context keys used by this package.
type ContextKey string

// ClientKey is the context key under which a CloudClient is stored.
const ClientKey ContextKey = "client"

// GetClient retrieves the cloud client from the context, if ctx is nil it returns nil.
func GetClient(ctx *context.Context) CloudClient {
	if ctx == nil || *ctx == nil {
		return nil
	}

	ctxStore, _ := (*ctx).Value(ClientKey).(CloudClient)

	return ctxStore
}

type CloudClient interface {
	CreateMultipartUpload(storagePath *string) (*string, error)
	UploadPart(input *CloudUploadPartInput) (*CloudUploadPartReponse, error)
	CompletePartUpload(input *CloudUploadPartInput) (*CloudUploadPartCompleted, error)
	UploadFile(fileName *string, filePath string) error
	DownloadFile(fileName *string, downloadPath string) error
}

type CloudUploadPartInput struct {
	PartNumber  int
	UploadId    string
	Buffer      *[]byte
	StoragePath *string
	Parts       *[]*CloudUploadPartReponse
}

type CloudUploadPartReponse struct {
	ETag       *string
	PartNumber *int64
}

type CloudUploadPartCompleted struct {
	Recording_Url *string
}
