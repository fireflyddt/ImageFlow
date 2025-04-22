package utils

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv" // Needed for parsing boolean env var
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config" // AWS SDK config package
	"github.com/aws/aws-sdk-go-v2/credentials" // For static credentials if needed
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// --- Existing Structs and Interfaces (unchanged) ---

// S3Object represents an object in S3 storage
type S3Object struct {
	Key  string
	Size int64
}

// StorageProvider defines the interface for storage operations
type StorageProvider interface {
	Store(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	// Add ListObjects to the interface if S3Storage implements it and it should be generally available
	// ListObjects(ctx context.Context, prefix string) ([]S3Object, error) // Uncomment if needed
}

// --- LocalStorage Implementation (unchanged) ---

type LocalStorage struct {
	BasePath string
}

func NewLocalStorage(basePath string) (*LocalStorage, error) {
	dirs := []string{
		filepath.Join(basePath, "original", "landscape"),
		filepath.Join(basePath, "original", "portrait"),
		filepath.Join(basePath, "landscape", "webp"),
		filepath.Join(basePath, "landscape", "avif"),
		filepath.Join(basePath, "portrait", "webp"),
		filepath.Join(basePath, "portrait", "avif"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %v", dir, err)
		}
	}

	return &LocalStorage{BasePath: basePath}, nil
}

func (ls *LocalStorage) Store(ctx context.Context, key string, data []byte) error {
	fullPath := filepath.Join(ls.BasePath, key)
	dir := filepath.Dir(fullPath)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %v", dir, err)
	}

	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write file %s: %v", fullPath, err)
	}

	log.Printf("File stored locally: %s", key)
	return nil
}

func (ls *LocalStorage) Get(ctx context.Context, key string) ([]byte, error) {
	fullPath := filepath.Join(ls.BasePath, key)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		// Handle file not found specifically if needed, otherwise wrap the error
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("local file not found %s: %w", fullPath, err) // Or return a specific error type
		}
		return nil, fmt.Errorf("failed to read local file %s: %w", fullPath, err)
	}
	return data, nil
}

func (ls *LocalStorage) Delete(ctx context.Context, key string) error {
	fullPath := filepath.Join(ls.BasePath, key)
	err := os.Remove(fullPath)
	if err != nil && !os.IsNotExist(err) { // Don't error if file already doesn't exist
		return fmt.Errorf("failed to delete local file %s: %w", fullPath, err)
	}
	log.Printf("File deleted locally: %s", key)
	return nil
}

// --- S3 Client Initialization (NEW/MODIFIED) ---

// S3Client is the global S3 client instance. Consider initializing it within InitStorage instead of globally.
var S3Client *s3.Client

// InitS3Client initializes the S3 client based on environment variables.
// It now supports path-style access via S3_USE_PATH_STYLE env var.
func InitS3Client() error {
	ctx := context.Background()
	endpoint := os.Getenv("S3_ENDPOINT")
	region := os.Getenv("AWS_REGION") // Or S3_REGION if you prefer
	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	usePathStyleStr := os.Getenv("S3_USE_PATH_STYLE") // New environment variable

	// Basic validation
	if endpoint == "" || region == "" || accessKeyID == "" || secretAccessKey == "" {
		return fmt.Errorf("S3 environment variables missing: S3_ENDPOINT, AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY must be set")
	}

	// Parse the path style flag (default to false)
	usePathStyle, _ := strconv.ParseBool(usePathStyleStr) // Error ignored, defaults to false if parsing fails or var is empty

	// Custom endpoint resolver function
	endpointResolver := aws.EndpointResolverWithOptionsFunc(
		func(service, resolvedRegion string, options ...interface{}) (aws.Endpoint, error) {
			// Use the provided endpoint for S3 service
			if service == s3.ServiceID {
				log.Printf("Using custom S3 endpoint: %s (Signing Region: %s)", endpoint, region)
				return aws.Endpoint{
					URL:           endpoint, // The custom endpoint URL
					SigningRegion: region,   // Region for signing requests
					// PartitionID: "aws", // Usually "aws" for AWS S3, might differ for compatible storage
					// Source: aws.EndpointSourceCustom, // Optional: indicates custom source
				}, nil
			}
			// Fallback to default resolver for other services (if any)
			return aws.Endpoint{}, &aws.EndpointNotFoundError{}
		})

	// Load AWS configuration
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
		config.WithEndpointResolverWithOptions(endpointResolver),
	)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// --- Create S3 Client with Path Style Option ---
	// Pass a function to s3.NewFromConfig to customize client options
	S3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = usePathStyle // <<< THIS IS THE KEY CHANGE
		// You can set other client-specific options here if needed
		// o.Region = region // Region is usually set via config.WithRegion above
	})

	log.Printf("S3 Client initialized. Endpoint: %s, Region: %s, PathStyle: %t", endpoint, region, usePathStyle)
	return nil
}

// --- S3Storage Implementation (Minor change in NewS3Storage) ---

// S3Storage implements StorageProvider for S3-compatible storage
type S3Storage struct {
	client *s3.Client
	bucket string
}

// NewS3Storage creates a new S3 storage provider.
// It ensures the S3 client is initialized before creating the struct.
func NewS3Storage() (*S3Storage, error) {
	// Ensure client is initialized (idempotent or called once externally)
	if S3Client == nil {
		if err := InitS3Client(); err != nil {
			return nil, fmt.Errorf("failed to initialize S3 client: %w", err)
		}
	}

	bucketName := os.Getenv("S3_BUCKET")
	if bucketName == "" {
		return nil, fmt.Errorf("S3_BUCKET environment variable is not set")
	}

	return &S3Storage{
		client: S3Client, // Use the initialized global client
		bucket: bucketName,
	}, nil
}

// Store uploads data to S3. (Unchanged logic)
func (s *S3Storage) Store(ctx context.Context, key string, data []byte) error {
	log.Printf("Storing to S3: bucket=%s, key=%s, size=%d bytes", s.bucket, key, len(data))

	contentType := "application/octet-stream"
	ext := strings.ToLower(filepath.Ext(key))
	switch ext {
	case ".jpg", ".jpeg":
		contentType = "image/jpeg"
	case ".png":
		contentType = "image/png" // Added PNG for completeness
	case ".gif":
		contentType = "image/gif" // Added GIF
	case ".webp":
		contentType = "image/webp"
	case ".avif":
		contentType = "image/avif"
	case ".svg":
		contentType = "image/svg+xml" // Added SVG
	}

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(data),
		ContentType:  aws.String(contentType),
		ACL:          types.ObjectCannedACLPublicRead, // Consider if public-read is always desired
		CacheControl: aws.String("public, max-age=31536000"), // Cache for 1 year
	})
	if err != nil {
		log.Printf("Failed to store object in S3: %v", err)
		return fmt.Errorf("failed to store object '%s' in bucket '%s': %w", key, s.bucket, err)
	}

	// URL Logging (unchanged, relies on S3_ENDPOINT and CUSTOM_DOMAIN)
	customDomain := os.Getenv("CUSTOM_DOMAIN")
	s3Endpoint := os.Getenv("S3_ENDPOINT")
	var url string
	if customDomain != "" {
		// Assumes custom domain points directly to bucket contents (virtual-hosted style or CDN)
		url = fmt.Sprintf("%s/%s", strings.TrimSuffix(customDomain, "/"), key)
	} else if s3Endpoint != "" {
		// This format naturally works for path-style when S3_ENDPOINT is the base endpoint
		// e.g., http://minio:9000/my-bucket/my-key
		// It also works for virtual-hosted if S3_ENDPOINT includes the bucket, but that's less common.
		// For standard AWS virtual-hosted, the SDK handles the URL internally.
		url = fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(s3Endpoint, "/"), s.bucket, key)
	} else {
		url = "[URL unavailable - S3_ENDPOINT or CUSTOM_DOMAIN not set]"
	}
	log.Printf("Successfully stored object in S3: %s (Public URL might be: %s)", key, url)
	return nil
}

// Get retrieves data from S3. (Unchanged logic)
func (s *S3Storage) Get(ctx context.Context, key string) ([]byte, error) {
	log.Printf("Getting object from S3: bucket=%s, key=%s", s.bucket, key)
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// Example of checking for NoSuchKey error specifically
		// var nsk *types.NoSuchKey
		// if errors.As(err, &nsk) {
		// 	 return nil, fmt.Errorf("object not found in S3: %s/%s", s.bucket, key) // Return a more specific error
		// }
		return nil, fmt.Errorf("failed to get object '%s' from bucket '%s': %w", key, s.bucket, err)
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read object '%s' body from S3: %w", key, err)
	}
	log.Printf("Successfully retrieved object from S3: %s (%d bytes)", key, len(data))
	return data, nil
}

// Delete removes an object from S3. (Unchanged logic)
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	log.Printf("Deleting object from S3: bucket=%s, key=%s", s.bucket, key)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// You might want to check if the error is because the key didn't exist and ignore it.
		// However, simply wrapping the error is often sufficient.
		return fmt.Errorf("failed to delete object '%s' from bucket '%s': %w", key, s.bucket, err)
	}
	log.Printf("Successfully deleted object from S3: %s", key)
	return nil
}

// ListObjects lists objects in S3 with the given prefix. (Unchanged logic)
func (s *S3Storage) ListObjects(ctx context.Context, prefix string) ([]S3Object, error) {
	log.Printf("Listing objects in S3: bucket=%s, prefix=%s", s.bucket, prefix)
	var objects []S3Object

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	pageCount := 0
	for paginator.HasMorePages() {
		pageCount++
		log.Printf("Listing objects page %d", pageCount)
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects page %d from bucket '%s' with prefix '%s': %w", pageCount, s.bucket, prefix, err)
		}

		for _, obj := range page.Contents {
			if obj.Key != nil && obj.Size != nil { // Basic check for nil pointers
				objects = append(objects, S3Object{
					Key:  *obj.Key,
					Size: *obj.Size,
				})
			}
		}
	}

	log.Printf("Found %d objects in S3 with prefix '%s'", len(objects), prefix)
	return objects, nil
}

// --- Storage Configuration and Initialization (Unchanged logic) ---

// StorageConfig represents the storage configuration
type StorageConfig struct {
	Type      string // "local" or "s3"
	LocalPath string // base path for local storage
}

// NewStorageProvider creates a new storage provider based on configuration
func NewStorageProvider(cfg StorageConfig) (StorageProvider, error) {
	log.Printf("Creating storage provider of type: %s", cfg.Type)
	switch cfg.Type {
	case "local":
		if cfg.LocalPath == "" {
			return nil, fmt.Errorf("local storage path (LOCAL_STORAGE_PATH) is required when STORAGE_TYPE is 'local'")
		}
		return NewLocalStorage(cfg.LocalPath)
	case "s3":
		// NewS3Storage handles required env vars internally now
		return NewS3Storage()
	default:
		return nil, fmt.Errorf("unsupported storage type: '%s'. Use 'local' or 's3'", cfg.Type)
	}
}

// Global storage instance
var Storage StorageProvider

// InitStorage initializes the global storage provider
func InitStorage() error {
	log.Println("Initializing storage...")
	storageType := os.Getenv("STORAGE_TYPE")
	if storageType == "" {
		storageType = "local" // Default to local if not specified
		log.Println("STORAGE_TYPE not set, defaulting to 'local'")
	}

	storageConfig := StorageConfig{
		Type:      storageType,
		LocalPath: os.Getenv("LOCAL_STORAGE_PATH"), // Will be ignored if type is s3
	}

	var err error
	Storage, err = NewStorageProvider(storageConfig)
	if err != nil {
		log.Printf("Error initializing storage: %v", err)
		return fmt.Errorf("storage initialization failed: %w", err)
	}

	log.Println("Storage initialized successfully.")
	return nil
}
