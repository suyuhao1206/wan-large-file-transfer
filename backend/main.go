package main

import (
    "context"
    "crypto/sha256"
    "encoding/base64"
    "encoding/hex"
    "fmt"
    "database/sql"
    crand "crypto/rand"
    "math/big"
    "log"
    "net/http"
    "net/url"
    "os"
    "path/filepath"
    "regexp"
    "sort"
    "strings"
    "sync"
    "time"
    "strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
    "github.com/gin-contrib/cors"
    "github.com/gin-gonic/gin"
    "github.com/tus/tusd/v2/pkg/filestore"
    "github.com/tus/tusd/v2/pkg/handler"
    "github.com/tus/tusd/v2/pkg/s3store"
    _ "github.com/lib/pq"
)

const (
	maxS3MultipartParts      = 10000
	maxS3MultipartPartSize   = int64(5 * 1024 * 1024 * 1024)
	maxS3PartCopyConcurrency = 16
	mergeTaskCleanupDelay     = 5 * time.Minute
	mergeTaskMinTimeout       = 30 * time.Minute
	mergeTaskBaseTimeout      = 10 * time.Minute
	mergeTaskPerChunkTimeout  = 5 * time.Second
	mergeTaskStatusProcessing = "processing"
	mergeTaskStatusSuccess    = "success"
	mergeTaskStatusFailed     = "failed"
)

// Global Storage
var (
    codeToUpload = make(map[string]UploadRecord)
    s3KeyCache   = make(map[string]string)
	uploadMetrics = make(map[string]UploadMetric)
	mergeTasks    sync.Map
    mu           sync.RWMutex
	metricsMu    sync.RWMutex
    
    // Global components
    composer        *handler.StoreComposer
    dataStore       handler.DataStore
    s3Client        *s3.Client
    s3PresignClient *s3.PresignClient
    s3Bucket        string
    s3Endpoint      string // Global variable for endpoint
    uploadDir       string
    db              *sql.DB
    ttlMinutes      int
	maxDownloads    int
	bandwidthBaselineMbps float64
	codeRegex       = regexp.MustCompile(`^[A-Za-z0-9._-]{4,128}$`)
	apiKey          string // Legacy API key for authentication (from env)
	adminKey        string // Admin key for managing API keys (optional)
)

type UploadRecord struct {
	UploadID string `json:"upload_id"`
	Filename string `json:"filename"`
}

type FinalizeMultipartChunk struct {
	UploadID string `json:"upload_id" binding:"required"`
	Index    int    `json:"index"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	Size     int64  `json:"size"`
}

type FinalizeMultipartRequest struct {
	Filename  string                   `json:"filename" binding:"required"`
	Filetype  string                   `json:"filetype"`
	TotalSize int64                    `json:"total_size" binding:"required"`
	Chunks    []FinalizeMultipartChunk `json:"chunks" binding:"required"`
}

type MergeTask struct {
	mu                  sync.RWMutex
	TaskID              string
	Status              string
	UploadID            string
	Code                string
	Filename            string
	OwnerHash           string
	Error               string
	ChunkUploadIDs      []string
	S3MultipartUploadID string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type UploadMetric struct {
	Bytes    int64
	Intervals []UploadMetricInterval
}

type UploadMetricInterval struct {
	Start time.Time
	End   time.Time
}

type UploadMetricSummary struct {
	Bytes                int64
	StartedAt            time.Time
	FinishedAt           time.Time
	Duration             time.Duration
	AverageMbps          float64
	BandwidthUtilization float64
	BandwidthBaselineMbps float64
}

func shareCodeExpiresAt() sql.NullTime {
	if ttlMinutes <= 0 {
		return sql.NullTime{}
	}
	return sql.NullTime{
		Time:  time.Now().UTC().Add(time.Duration(ttlMinutes) * time.Minute),
		Valid: true,
	}
}

func main() {
    // Environment variables
    s3Endpoint = os.Getenv("S3_ENDPOINT")
    s3Bucket = os.Getenv("S3_BUCKET") // Assign to global
    s3AccessKey := os.Getenv("S3_ACCESS_KEY")
    s3SecretKey := os.Getenv("S3_SECRET_KEY")
    s3Region := os.Getenv("S3_REGION")
    uploadDir = os.Getenv("TUSD_UPLOAD_DIR") // Assign to global
    dbHost := os.Getenv("DB_HOST")
    dbUser := os.Getenv("DB_USER")
    dbPassword := os.Getenv("DB_PASSWORD")
    dbName := os.Getenv("DB_NAME")
    ttlMinutesStr := os.Getenv("SHARE_CODE_TTL_MINUTES")
    maxDownloadsStr := os.Getenv("SHARE_CODE_MAX_DOWNLOADS")
    ttlMinutes = 120
    if n, err := strconv.Atoi(ttlMinutesStr); err == nil && n >= 0 { ttlMinutes = n }
    maxDownloads = 10000
    if n, err := strconv.Atoi(maxDownloadsStr); err == nil && n > 0 { maxDownloads = n }
	bandwidthBaselineMbps = 100
	if n, err := strconv.ParseFloat(os.Getenv("BANDWIDTH_BASELINE_MBPS"), 64); err == nil && n > 0 {
		bandwidthBaselineMbps = n
	}
    corsOrigins := os.Getenv("CORS_ORIGINS")
    enableDebug := strings.ToLower(os.Getenv("ENABLE_DEBUG_ENDPOINTS")) == "true"
    apiKey = os.Getenv("API_KEY") // Legacy API key from environment (optional)
    adminKey = os.Getenv("ADMIN_KEY") // Admin key for managing API keys (optional)

    if dbHost != "" && dbUser != "" && dbName != "" {
        dsn := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", dbUser, dbPassword, dbHost, dbName)
        if conn, err := sql.Open("postgres", dsn); err == nil {
            if err = conn.Ping(); err == nil {
                db = conn
                db.SetMaxOpenConns(20)
                db.SetMaxIdleConns(10)
                db.SetConnMaxLifetime(30 * time.Minute)
                _, _ = db.Exec(`CREATE TABLE IF NOT EXISTS share_codes (
                    code TEXT PRIMARY KEY,
                    upload_id TEXT NOT NULL,
                    filename TEXT NOT NULL,
                    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    expires_at TIMESTAMPTZ,
                    downloads INTEGER NOT NULL DEFAULT 0,
                    max_downloads INTEGER NOT NULL DEFAULT 0
                )`)
                _, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_share_codes_upload ON share_codes(upload_id)`)
                _, _ = db.Exec(`ALTER TABLE share_codes ADD COLUMN IF NOT EXISTS owner_key_hash TEXT`)
                _, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_share_codes_owner ON share_codes(owner_key_hash)`)
                _, _ = db.Exec(`CREATE TABLE IF NOT EXISTS upload_metric_intervals (
                    id BIGSERIAL PRIMARY KEY,
                    upload_id TEXT NOT NULL,
                    bytes BIGINT NOT NULL,
                    started_at TIMESTAMPTZ NOT NULL,
                    finished_at TIMESTAMPTZ NOT NULL,
                    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )`)
                _, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_upload_metric_intervals_upload ON upload_metric_intervals(upload_id)`)
                _, _ = db.Exec(`CREATE TABLE IF NOT EXISTS upload_metrics (
                    upload_id TEXT PRIMARY KEY,
                    bytes BIGINT NOT NULL DEFAULT 0,
                    started_at TIMESTAMPTZ,
                    finished_at TIMESTAMPTZ,
                    duration_ms BIGINT NOT NULL DEFAULT 0,
                    average_mbps DOUBLE PRECISION NOT NULL DEFAULT 0,
                    bandwidth_utilization DOUBLE PRECISION NOT NULL DEFAULT 0,
                    bandwidth_baseline_mbps DOUBLE PRECISION NOT NULL DEFAULT 100,
                    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )`)
                if ttlMinutes <= 0 {
                    if _, err := db.Exec(`UPDATE share_codes SET expires_at = NULL WHERE expires_at IS NOT NULL`); err != nil {
                        log.Printf("Failed to disable share code expiration: %v", err)
                    }
                }
                _, _ = db.Exec(`CREATE TABLE IF NOT EXISTS upload_owners (
                    upload_id TEXT PRIMARY KEY,
                    owner_key_hash TEXT NOT NULL,
                    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )`)
                _, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_upload_owners_owner ON upload_owners(owner_key_hash)`)
                _, _ = db.Exec(`CREATE TABLE IF NOT EXISTS merge_tasks (
                    task_id TEXT PRIMARY KEY,
                    upload_id TEXT NOT NULL,
                    filename TEXT NOT NULL,
                    owner_key_hash TEXT,
                    status TEXT NOT NULL,
                    code TEXT,
                    error TEXT,
                    chunk_upload_ids TEXT NOT NULL DEFAULT '',
                    s3_multipart_upload_id TEXT,
                    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )`)
                _, _ = db.Exec(`ALTER TABLE merge_tasks ADD COLUMN IF NOT EXISTS chunk_upload_ids TEXT NOT NULL DEFAULT ''`)
                _, _ = db.Exec(`ALTER TABLE merge_tasks ADD COLUMN IF NOT EXISTS s3_multipart_upload_id TEXT`)
                _, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_merge_tasks_status ON merge_tasks(status)`)
                _, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_merge_tasks_owner ON merge_tasks(owner_key_hash)`)
                
                // Create API keys table
                _, _ = db.Exec(`CREATE TABLE IF NOT EXISTS api_keys (
                    id SERIAL PRIMARY KEY,
                    key_hash TEXT NOT NULL UNIQUE,
                    key_prefix TEXT NOT NULL,
                    name TEXT,
                    description TEXT,
                    is_active BOOLEAN NOT NULL DEFAULT true,
                    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    last_used_at TIMESTAMPTZ,
                    expires_at TIMESTAMPTZ
                )`)
                _, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash)`)
                _, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_api_keys_active ON api_keys(is_active)`)
            }
        }
    }

	// Configure Storage Backend
	if s3Endpoint != "" && s3Bucket != "" {
		log.Printf("Using S3 Storage. Endpoint: %s, Bucket: %s", s3Endpoint, s3Bucket)

		creds := credentials.NewStaticCredentialsProvider(s3AccessKey, s3SecretKey, "")
		cfg, err := config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(s3Region),
			config.WithCredentialsProvider(creds),
			config.WithEndpointResolverWithOptions(
				aws.EndpointResolverWithOptionsFunc(
					func(service, region string, options ...interface{}) (aws.Endpoint, error) {
						return aws.Endpoint{URL: s3Endpoint}, nil
					}),
			),
		)
		if err != nil {
			log.Fatalf("Failed to load AWS config: %v", err)
		}

		s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.UsePathStyle = true
		})

		// Configure Presign Client (Use Public Endpoint if available)
		presignS3Client := s3Client
		s3PublicEndpoint := os.Getenv("S3_PUBLIC_ENDPOINT")
		if s3PublicEndpoint != "" {
			log.Printf("Using Public S3 Endpoint for presigning: %s", s3PublicEndpoint)
			
			// Create a separate config for public endpoint
			publicCfg := cfg
			publicCfg.EndpointResolverWithOptions = aws.EndpointResolverWithOptionsFunc(
				func(service, region string, options ...interface{}) (aws.Endpoint, error) {
					return aws.Endpoint{URL: s3PublicEndpoint}, nil
				})

			presignS3Client = s3.NewFromConfig(publicCfg, func(o *s3.Options) {
				o.UsePathStyle = true
			})
		}
		s3PresignClient = s3.NewPresignClient(presignS3Client)

		// Auto create bucket
		_, err = s3Client.HeadBucket(context.TODO(), &s3.HeadBucketInput{
			Bucket: aws.String(s3Bucket),
		})
		if err != nil {
			log.Printf("Bucket %s not found, creating...", s3Bucket)
			_, err = s3Client.CreateBucket(context.TODO(), &s3.CreateBucketInput{
				Bucket: aws.String(s3Bucket),
			})
			if err != nil {
				log.Fatalf("Failed to create bucket %s: %v", s3Bucket, err)
			}
		}

		store := s3store.New(s3Bucket, s3Client)
		dataStore = store
		composer = handler.NewStoreComposer()
		store.UseIn(composer)
	} else {
		log.Println("Using File Storage")
		if uploadDir == "" {
			uploadDir = "./uploads"
		}
		if abs, err := filepath.Abs(uploadDir); err == nil {
			uploadDir = abs
		}
		os.MkdirAll(uploadDir, os.ModePerm)
		store := filestore.New(uploadDir)
		dataStore = store
		composer = handler.NewStoreComposer()
		store.UseIn(composer)
	}

	recoverInterruptedMergeTasks()

	// Create tusd handler
	tusConfig := handler.Config{
		BasePath:                "/files/",
		StoreComposer:           composer,
		NotifyCompleteUploads:   true,
		NotifyTerminatedUploads: true,
	}

	tusHandler, err := handler.NewHandler(tusConfig)
	if err != nil {
		log.Fatalf("Unable to create tus handler: %s", err)
	}

	log.Printf("Tusd CompleteUploads channel initialized.")

	// Listen for completed uploads
    go func() {
        for {
            event := <-tusHandler.CompleteUploads
            uploadID := event.Upload.ID

			if event.Upload.IsPartial {
				log.Printf("Event: Partial upload completed - ID: %s", uploadID)
				continue
			}

			if event.Upload.IsFinal {
				aggregateUploadMetrics(uploadID, event.Upload.PartialUploads)
			}
            
            filename := extractFilename(event.Upload.MetaData)

			if isBusinessMultipartChunk(event.Upload.MetaData) {
				log.Printf("Event: Business multipart chunk completed - ID: %s, Filename: %s", uploadID, filename)
				continue
			}

			mu.Lock()
			codeToUpload[uploadID] = UploadRecord{
				UploadID: uploadID,
				Filename: filename,
			}
            mu.Unlock()

			// Generate a short code for the new upload
			var shortCode string
			if db != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ownerHash := ownerHashForUpload(ctx, uploadID)
				
				// Retry loop for short code generation
				for i := 0; i < 5; i++ {
					shortCode = genCode(8) // Generate 8-char code
					exp := shareCodeExpiresAt()
					
					// Try insert
					res, err := db.ExecContext(ctx, 
						"INSERT INTO share_codes(code, upload_id, filename, owner_key_hash, expires_at, max_downloads, downloads) VALUES($1,$2,$3,$4,$5,$6,0) ON CONFLICT (code) DO NOTHING",
						shortCode, uploadID, filename, nullableString(ownerHash), exp, maxDownloads)
					
					if err == nil {
						if rows, _ := res.RowsAffected(); rows > 0 {
							log.Printf("Generated short code %s for upload %s", shortCode, uploadID)
							break
						}
					}
					shortCode = "" // Reset if failed
				}
				
				// If short code generation failed (very unlikely), fallback to UploadID? 
				// Better to leave it and let get-code handle it or use UploadID as last resort.
				if shortCode == "" {
					// Fallback to inserting UploadID as code to ensure record exists
					_, _ = db.ExecContext(ctx, "INSERT INTO share_codes(code, upload_id, filename, owner_key_hash, max_downloads) VALUES($1,$2,$3,$4,$5) ON CONFLICT (code) DO UPDATE SET filename=EXCLUDED.filename, owner_key_hash=COALESCE(share_codes.owner_key_hash, EXCLUDED.owner_key_hash)", uploadID, uploadID, filename, nullableString(ownerHash), maxDownloads)
				}
			}

			if s3Client != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if key, err := resolveS3ObjectKey(ctx, uploadID); err == nil {
					log.Printf("Cached S3 key for upload %s: %s", uploadID, key)
				} else {
					log.Printf("Failed to cache S3 key for upload %s: %v", uploadID, err)
				}
				cancel()
			}

            log.Printf("Event: Upload completed - ID: %s, Filename: %s", uploadID, filename)
        }
    }()

	go func() {
		for {
			event := <-tusHandler.TerminatedUploads
			uploadID := event.Upload.ID
			clearUploadRuntimeState(uploadID)
			log.Printf("Event: Upload terminated - ID: %s", uploadID)
		}
	}()

    // Start Gin Server
    gin.SetMode(gin.ReleaseMode)
    r := gin.Default()
    if err := r.SetTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}); err != nil {
        log.Printf("SetTrustedProxies error: %v", err)
    }

    // CORS
    var origins []string
    if corsOrigins == "" || corsOrigins == "*" {
        origins = []string{"*"}
    } else {
        parts := strings.Split(corsOrigins, ",")
        for _, p := range parts {
            p = strings.TrimSpace(p)
            if p != "" { origins = append(origins, p) }
        }
        if len(origins) == 0 { origins = []string{"*"} }
    }
    r.Use(cors.New(cors.Config{
        AllowOrigins:  origins,
        AllowMethods:  []string{"GET", "POST", "PATCH", "HEAD", "OPTIONS", "DELETE"},
        AllowHeaders:  []string{"Origin", "Content-Type", "Upload-Length", "Upload-Metadata", "Upload-Concat", "Tus-Resumable", "Upload-Offset", "Authorization", "X-API-Key", "X-Admin-Key"},
        ExposeHeaders: []string{"Upload-Length", "Upload-Metadata", "Upload-Concat", "Tus-Resumable", "Upload-Offset", "Location", "Tus-Version"},
        AllowCredentials: false,
    }))

	// Files Handler (Custom GET + Tusd)
	r.Any("/files/*any", apiKeyAuth(), func(c *gin.Context) {
		path := c.Param("any")
		uploadID := strings.TrimPrefix(path, "/")

        // 1. Handle Download (GET)
        if c.Request.Method == "GET" && uploadID != "" {
            filename := "download"
			directBusinessChunk := false
			
			mu.RLock()
			record, exists := codeToUpload[uploadID]
			mu.RUnlock()
			
			if exists {
				filename = record.Filename
			} else {
				if upload, err := dataStore.GetUpload(context.Background(), uploadID); err == nil {
					if info, err := upload.GetInfo(context.Background()); err == nil {
						if isBusinessMultipartChunk(info.MetaData) {
							directBusinessChunk = true
						} else {
							filename = extractFilename(info.MetaData)
							mu.Lock()
							codeToUpload[uploadID] = UploadRecord{UploadID: uploadID, Filename: filename}
							mu.Unlock()
						}
					}
				}
			}

            code := c.Query("code")
            if db != nil {
                if code == "" {
                    code = uploadID
                }
                // Validate code format
                if !codeRegex.MatchString(code) {
                    c.JSON(400, gin.H{"error": "Invalid code format"})
                    return
                }

                var uid string
                var fname string
                var expires sql.NullTime
                var downloads int
                var maxd int
                
                ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
                defer cancel()

                err := db.QueryRowContext(ctx, "SELECT upload_id, filename, expires_at, downloads, max_downloads FROM share_codes WHERE code=$1", code).Scan(&uid, &fname, &expires, &downloads, &maxd)
                if err == nil {
                    // Allow Short Code in Path: If code matches but uploadID (path) is different,
                    // and path equals the code, then we are using the short code as the path.
                    if uid != uploadID {
                        if uploadID == code {
                            uploadID = uid
                        } else {
                            c.JSON(400, gin.H{"error": "Code does not match upload"})
                            return
                        }
                    }
                    
                    if expires.Valid && time.Now().UTC().After(expires.Time) {
                        c.JSON(410, gin.H{"error": "Code expired"})
                        return
                    }
                    res, uerr := db.ExecContext(ctx, "UPDATE share_codes SET downloads=downloads+1 WHERE code=$1 AND (max_downloads=0 OR downloads < max_downloads)", code)
                    if uerr != nil {
                        c.JSON(500, gin.H{"error": "Download counter update failed"})
                        return
                    }
                    if rows, _ := res.RowsAffected(); rows == 0 {
                        c.JSON(429, gin.H{"error": "Download limit reached"})
                        return
                    }
                    filename = fname
                } else {
                    upload, uerr := dataStore.GetUpload(context.Background(), uploadID)
                    if uerr != nil {
                        c.JSON(404, gin.H{"error": "Code not found"})
                        return
                    }
                    info, ierr := upload.GetInfo(context.Background())
                    if ierr != nil {
                        c.JSON(404, gin.H{"error": "Code not found"})
                        return
                    }
					if isBusinessMultipartChunk(info.MetaData) {
						c.JSON(404, gin.H{"error": "Code not found"})
						return
					}
                    fname = extractFilename(info.MetaData)
                    exp := shareCodeExpiresAt()
                    
                    // Auto-register
                    _, _ = db.ExecContext(ctx, "INSERT INTO share_codes(code, upload_id, filename, expires_at, max_downloads, downloads) VALUES($1,$2,$3,$4,$5,0) ON CONFLICT (code) DO UPDATE SET filename=EXCLUDED.filename WHERE share_codes.upload_id = EXCLUDED.upload_id", code, uploadID, fname, exp, maxDownloads)
                    
                    res, uerr2 := db.ExecContext(ctx, "UPDATE share_codes SET downloads=downloads+1 WHERE code=$1 AND (max_downloads=0 OR downloads < max_downloads)", code)
                    if uerr2 != nil {
                        c.JSON(500, gin.H{"error": "Download counter update failed"})
                        return
                    }
                    if rows, _ := res.RowsAffected(); rows == 0 {
                        c.JSON(429, gin.H{"error": "Download limit reached"})
                        return
                    }
                    filename = fname
                }
            }
			if db == nil && directBusinessChunk {
				c.JSON(404, gin.H{"error": "File not found"})
				return
			}

			// S3 Download (Redirect to Presigned URL)
			if s3Client != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				targetKey, err := resolveS3ObjectKey(ctx, uploadID)
				cancel()
				if err != nil {
					log.Printf("Failed to resolve S3 key for %s: %v", uploadID, err)
					c.JSON(404, gin.H{"error": "File not found"})
					return
				}

				presignReq, err := s3PresignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
					Bucket:                     aws.String(s3Bucket),
					Key:                        aws.String(targetKey),
					ResponseContentDisposition: aws.String(fmt.Sprintf("attachment; filename*=UTF-8''%s", url.QueryEscape(filename))),
				}, func(o *s3.PresignOptions) {
					o.Expires = 15 * time.Minute
				})

				if err != nil {
					log.Printf("Failed to presign S3 url: %v", err)
					c.Status(500)
					return
				}

				finalUrl := presignReq.URL
				c.Redirect(http.StatusFound, finalUrl)
				return
			} else {
				filePath := filepath.Join(uploadDir, uploadID)
				c.Header("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.QueryEscape(filename)))
				http.ServeFile(c.Writer, c.Request, filePath)
				return
			}
		}

		// 2. Handle Tusd Protocol (POST, PATCH, HEAD, etc.)
		// Or GET without ID (listing? not supported by tusd usually)
		// Pass to tusd
		metricStartedAt := time.Now()
		requestOffset := parseIntHeader(c.Request.Header.Get("Upload-Offset"))

		var ownerHash string
		if c.Request.Method == http.MethodPost {
			if value, exists := c.Get("api_key_hash"); exists {
				if hash, ok := value.(string); ok {
					ownerHash = hash
				}
			}
		}

		http.StripPrefix("/files/", tusHandler).ServeHTTP(c.Writer, c.Request)

		if c.Request.Method == http.MethodPatch && uploadID != "" && c.Writer.Status() >= 200 && c.Writer.Status() < 300 {
			metricFinishedAt := time.Now()
			responseOffset := parseIntHeader(c.Writer.Header().Get("Upload-Offset"))
			acceptedBytes := responseOffset - requestOffset
			if acceptedBytes <= 0 && c.Request.ContentLength > 0 {
				acceptedBytes = c.Request.ContentLength
			}
			recordUploadMetric(uploadID, acceptedBytes, metricStartedAt, metricFinishedAt)
		}

		if c.Request.Method == http.MethodDelete && uploadID != "" && c.Writer.Status() >= 200 && c.Writer.Status() < 300 {
			clearUploadRuntimeState(uploadID)
		}

		if c.Request.Method == http.MethodPost && ownerHash != "" && c.Writer.Status() >= 200 && c.Writer.Status() < 300 {
			if uploadID := uploadIDFromLocation(c.Writer.Header().Get("Location")); uploadID != "" {
				rememberUploadOwner(uploadID, ownerHash)
			}
		}
	})

    // Health check endpoint (no authentication required)
    r.GET("/api/health", func(c *gin.Context) {
        status := gin.H{
            "status": "ok",
            "timestamp": time.Now().UTC().Format(time.RFC3339),
        }
        
        // Check database connection
        if db != nil {
            ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
            defer cancel()
            if err := db.PingContext(ctx); err != nil {
                status["database"] = "error"
                status["database_error"] = err.Error()
                c.JSON(http.StatusServiceUnavailable, status)
                return
            }
            status["database"] = "ok"
        } else {
            status["database"] = "not_configured"
        }
        
        // Check S3 connection
        if s3Client != nil {
            ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
            defer cancel()
            _, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
                Bucket: aws.String(s3Bucket),
            })
            if err != nil {
                status["storage"] = "error"
                status["storage_error"] = err.Error()
            } else {
                status["storage"] = "ok"
                status["storage_type"] = "s3"
            }
        } else {
            status["storage"] = "ok"
            status["storage_type"] = "file"
        }
        
        c.JSON(http.StatusOK, status)
    })

    // Verify API key endpoint (no authentication required)
    r.POST("/api/verify-key", func(c *gin.Context) {
        var req struct {
            ApiKey   string `json:"api_key"`
            AdminKey string `json:"admin_key"`
        }
        
        if err := c.ShouldBindJSON(&req); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"valid": false, "error": "Invalid request"})
            return
        }
        
        // Check if it's admin key
        if req.AdminKey != "" && adminKey != "" && req.AdminKey == adminKey {
            c.JSON(http.StatusOK, gin.H{"valid": true, "is_admin": true})
            return
        }
        
        // Check if it's API key
        if req.ApiKey != "" {
            valid, err := verifyAPIKey(req.ApiKey)
            if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"valid": false, "error": "Verification error"})
                return
            }
            if valid {
                c.JSON(http.StatusOK, gin.H{"valid": true, "is_admin": false})
                return
            }
        }
        
        // If no API key system is configured, allow access
        if apiKey == "" && db == nil {
            c.JSON(http.StatusOK, gin.H{"valid": true, "is_admin": false, "no_auth_required": true})
            return
        }
        
        c.JSON(http.StatusOK, gin.H{"valid": false})
    })

    // API Routes
    api := r.Group("/api")
    api.Use(apiKeyAuth()) // Apply API key authentication to all API routes
    {
        api.POST("/finalize-multipart", finalizeMultipartUpload)
        api.GET("/merge-status/:task_id", getMergeStatus)
        api.POST("/get-code", getShareCode)
        api.GET("/files", listUserFiles)
        api.GET("/retrieve/:code", retrieveFile)
        if enableDebug {
            api.GET("/debug/list-objects", listObjects)
            api.GET("/debug/head/:id", headObject)
        }
    }

    // Admin Routes (for API key management)
    admin := r.Group("/api/admin")
    admin.Use(adminAuth()) // Admin authentication
    {
        admin.POST("/keys", createAPIKey)
        admin.GET("/keys", listAPIKeys)
        admin.GET("/files", listAdminFiles)
        admin.DELETE("/keys/:id", deleteAPIKey)
        admin.PATCH("/keys/:id", updateAPIKey)
    }

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Server listening on :%s", port)
	r.Run(":" + port)
}

// Hash API key for storage
func hashAPIKey(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

// Generate a new API key
func generateAPIKey() string {
	// Generate 32 random bytes
	b := make([]byte, 32)
	crand.Read(b)
	// Encode as base64 and add prefix
	encoded := base64.URLEncoding.EncodeToString(b)
	// Format: fcb_<base64>
	return "fcb_" + encoded
}

// Get API key prefix (first 8 chars + ...)
func getKeyPrefix(key string) string {
	if len(key) <= 12 {
		return key[:len(key)-4] + "****"
	}
	return key[:8] + "****"
}

// Verify API key against database
func verifyAPIKey(key string) (bool, error) {
	// If legacy environment variable API key is set, check it first
	if apiKey != "" && key == apiKey {
		return true, nil
	}

	// If no database, skip verification (development mode)
	if db == nil {
		return true, nil
	}

	// Hash the provided key
	keyHash := hashAPIKey(key)

	// Check database
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var isActive bool
	var expiresAt sql.NullTime
	err := db.QueryRowContext(ctx, 
		"SELECT is_active, expires_at FROM api_keys WHERE key_hash = $1", 
		keyHash).Scan(&isActive, &expiresAt)

	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// Check if key is active
	if !isActive {
		return false, nil
	}

	// Check expiration
	if expiresAt.Valid && time.Now().UTC().After(expiresAt.Time) {
		return false, nil
	}

	// Update last_used_at
	_, _ = db.ExecContext(ctx, 
		"UPDATE api_keys SET last_used_at = NOW() WHERE key_hash = $1", 
		keyHash)

	return true, nil
}

func resolveS3ObjectKey(ctx context.Context, uploadID string) (string, error) {
	mu.RLock()
	if cachedKey, exists := s3KeyCache[uploadID]; exists && cachedKey != "" {
		mu.RUnlock()
		return cachedKey, nil
	}
	mu.RUnlock()

	_, err := s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(uploadID),
	})
	if err == nil {
		cacheS3ObjectKey(uploadID, uploadID)
		return uploadID, nil
	}

	log.Printf("Key %s not found directly, trying prefix search...", uploadID)

	prefix := uploadID
	if len(prefix) > 32 {
		prefix = prefix[:32]
	}

	listOut, lerr := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s3Bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: 10,
	})
	if lerr != nil {
		return "", lerr
	}

	for _, obj := range listOut.Contents {
		if obj.Key == nil {
			continue
		}
		key := *obj.Key
		if strings.HasSuffix(key, ".info") {
			continue
		}
		cacheS3ObjectKey(uploadID, key)
		log.Printf("Resolved S3 key: %s -> %s", uploadID, key)
		return key, nil
	}

	return "", fmt.Errorf("object not found for upload %s", uploadID)
}

func cacheS3ObjectKey(uploadID string, key string) {
	mu.Lock()
	s3KeyCache[uploadID] = key
	mu.Unlock()
}

// API Key authentication middleware
func apiKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Allow OPTIONS requests (CORS preflight) without authentication
		if c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}

		// If no API key system is configured (no env var and no DB), skip authentication
		if apiKey == "" && db == nil && adminKey == "" {
			c.Next()
			return
		}

		// Check for API key in header (X-API-Key or Authorization)
		providedKey := c.GetHeader("X-API-Key")
		if providedKey == "" {
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				providedKey = strings.TrimPrefix(authHeader, "Bearer ")
			} else if strings.HasPrefix(authHeader, "ApiKey ") {
				providedKey = strings.TrimPrefix(authHeader, "ApiKey ")
			}
		}

		// Also check query parameter as fallback
		if providedKey == "" {
			providedKey = c.Query("api_key")
		}

		// Check if Admin Key is provided (allow admin to use all features)
		adminKeyProvided := c.GetHeader("X-Admin-Key")
		if adminKeyProvided == "" {
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				adminKeyProvided = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}
		if adminKeyProvided == "" {
			adminKeyProvided = c.Query("admin_key")
		}
		
		// Verify Admin Key first (highest priority)
		if adminKeyProvided != "" && adminKey != "" && adminKeyProvided == adminKey {
			log.Printf("Admin key authenticated for %s %s", c.Request.Method, c.Request.URL.Path)
			c.Set("is_admin", true)
			c.Next()
			return
		}

		// If no API key provided, return error
		if providedKey == "" {
			log.Printf("Missing API key for %s %s", c.Request.Method, c.Request.URL.Path)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing API key"})
			c.Abort()
			return
		}

		// Verify API key
		valid, err := verifyAPIKey(providedKey)
		if err != nil {
			log.Printf("API key verification error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			c.Abort()
			return
		}

		if !valid {
			log.Printf("Invalid API key for %s %s", c.Request.Method, c.Request.URL.Path)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired API key"})
			c.Abort()
			return
		}

		log.Printf("API key authenticated for %s %s", c.Request.Method, c.Request.URL.Path)
		c.Set("api_key_hash", hashAPIKey(providedKey))
		c.Set("is_admin", false)
		c.Next()
	}
}

// Admin authentication middleware
func adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// If admin key is not set, allow localhost only (for development)
		if adminKey == "" {
			clientIP := c.ClientIP()
			if clientIP != "127.0.0.1" && clientIP != "::1" && !strings.HasPrefix(clientIP, "192.168.") && !strings.HasPrefix(clientIP, "10.") && !strings.HasPrefix(clientIP, "172.") {
				c.JSON(http.StatusForbidden, gin.H{"error": "Admin access restricted to local network"})
				c.Abort()
				return
			}
			c.Next()
			return
		}

		// Check admin key
		providedKey := c.GetHeader("X-Admin-Key")
		if providedKey == "" {
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				providedKey = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}

		if providedKey == "" {
			providedKey = c.Query("admin_key")
		}

		if providedKey != adminKey {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid admin key"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// API Key Management Handlers
type APIKeyRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ExpiresDays int    `json:"expires_days"` // 0 means never expires
}

type APIKeyResponse struct {
	ID          int       `json:"id"`
	Key         string    `json:"key"` // Only returned on creation
	KeyPrefix   string    `json:"key_prefix"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

type AdminFileRecord struct {
	Code                  string     `json:"code"`
	UploadID              string     `json:"upload_id"`
	Filename              string     `json:"filename"`
	CreatedAt             time.Time  `json:"created_at"`
	ExpiresAt             *time.Time `json:"expires_at"`
	Downloads             int        `json:"downloads"`
	MaxDownloads          int        `json:"max_downloads"`
	Status                string     `json:"status"`
	UploadBytes           int64      `json:"upload_bytes"`
	MetricStartedAt       *time.Time `json:"metric_started_at"`
	MetricFinishedAt      *time.Time `json:"metric_finished_at"`
	UploadDurationMs      int64      `json:"upload_duration_ms"`
	AverageMbps           float64    `json:"average_mbps"`
	BandwidthUtilization  float64    `json:"bandwidth_utilization"`
	BandwidthBaselineMbps float64    `json:"bandwidth_baseline_mbps"`
}

func createAPIKey(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	var req APIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Generate new API key
	newKey := generateAPIKey()
	keyHash := hashAPIKey(newKey)
	keyPrefix := getKeyPrefix(newKey)

	// Calculate expiration
	var expiresAt sql.NullTime
	if req.ExpiresDays > 0 {
		exp := time.Now().UTC().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
		expiresAt = sql.NullTime{Time: exp, Valid: true}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id int
	err := db.QueryRowContext(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, name, description, expires_at)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		keyHash, keyPrefix, req.Name, req.Description, expiresAt).Scan(&id)

	if err != nil {
		log.Printf("Failed to create API key: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create API key"})
		return
	}

	c.JSON(http.StatusOK, APIKeyResponse{
		ID:          id,
		Key:         newKey, // Only return full key on creation
		KeyPrefix:   keyPrefix,
		Name:        req.Name,
		Description: req.Description,
		IsActive:    true,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   func() *time.Time { if expiresAt.Valid { return &expiresAt.Time } else { return nil } }(),
	})
}

func listAPIKeys(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx,
		`SELECT id, key_prefix, name, description, is_active, created_at, last_used_at, expires_at
		 FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		log.Printf("Failed to list API keys: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list API keys"})
		return
	}
	defer rows.Close()

	var keys []APIKeyResponse
	for rows.Next() {
		var key APIKeyResponse
		var lastUsedAt sql.NullTime
		var expiresAt sql.NullTime

		err := rows.Scan(&key.ID, &key.KeyPrefix, &key.Name, &key.Description,
			&key.IsActive, &key.CreatedAt, &lastUsedAt, &expiresAt)
		if err != nil {
			continue
		}

		if lastUsedAt.Valid {
			key.LastUsedAt = &lastUsedAt.Time
		}
		if expiresAt.Valid {
			key.ExpiresAt = &expiresAt.Time
		}

		keys = append(keys, key)
	}

	c.JSON(http.StatusOK, gin.H{"keys": keys})
}

func listAdminFiles(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	files, err := querySharedFiles(ctx, "", true)
	if err != nil {
		log.Printf("Failed to list uploaded files: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list files"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"files": files})
}

func listUserFiles(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	if isAdmin, _ := c.Get("is_admin"); isAdmin == true {
		listAdminFiles(c)
		return
	}

	value, exists := c.Get("api_key_hash")
	ownerHash, ok := value.(string)
	if !exists || !ok || ownerHash == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "API key ownership is required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	files, err := querySharedFiles(ctx, ownerHash, false)
	if err != nil {
		log.Printf("Failed to list user files: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list files"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"files": files})
}

func querySharedFiles(ctx context.Context, ownerHash string, includeAll bool) ([]AdminFileRecord, error) {
	args := []interface{}{}
	whereClause := ""
	if !includeAll {
		whereClause = "WHERE sc.owner_key_hash = $1"
		args = append(args, ownerHash)
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT sc.code, sc.upload_id, sc.filename, sc.created_at, sc.expires_at, sc.downloads, sc.max_downloads,
			CASE
				WHEN sc.expires_at IS NOT NULL AND sc.expires_at < NOW() THEN 'expired'
				WHEN sc.max_downloads > 0 AND sc.downloads >= sc.max_downloads THEN 'download_limit'
				ELSE 'active'
			END AS status,
			COALESCE(um.bytes, 0) AS upload_bytes,
			um.started_at AS metric_started_at,
			um.finished_at AS metric_finished_at,
			COALESCE(um.duration_ms, 0) AS upload_duration_ms,
			COALESCE(um.average_mbps, 0) AS average_mbps,
			COALESCE(um.bandwidth_utilization, 0) AS bandwidth_utilization,
			COALESCE(um.bandwidth_baseline_mbps, 0) AS bandwidth_baseline_mbps
		FROM share_codes sc
		LEFT JOIN upload_metrics um ON um.upload_id = sc.upload_id
		%s
		ORDER BY sc.created_at DESC
		LIMIT 500
	`, whereClause), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	files := make([]AdminFileRecord, 0)
	for rows.Next() {
		var file AdminFileRecord
		var expiresAt sql.NullTime
		var metricStartedAt sql.NullTime
		var metricFinishedAt sql.NullTime

		err := rows.Scan(
			&file.Code,
			&file.UploadID,
			&file.Filename,
			&file.CreatedAt,
			&expiresAt,
			&file.Downloads,
			&file.MaxDownloads,
			&file.Status,
			&file.UploadBytes,
			&metricStartedAt,
			&metricFinishedAt,
			&file.UploadDurationMs,
			&file.AverageMbps,
			&file.BandwidthUtilization,
			&file.BandwidthBaselineMbps,
		)
		if err != nil {
			log.Printf("Failed to scan shared file row: %v", err)
			continue
		}

		if expiresAt.Valid {
			exp := expiresAt.Time
			file.ExpiresAt = &exp
		}
		if metricStartedAt.Valid {
			startedAt := metricStartedAt.Time
			file.MetricStartedAt = &startedAt
		}
		if metricFinishedAt.Valid {
			finishedAt := metricFinishedAt.Time
			file.MetricFinishedAt = &finishedAt
		}

		files = append(files, file)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return files, nil
}

func deleteAPIKey(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	id := c.Param("id")
	idInt, err := strconv.Atoi(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key ID"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := db.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", idInt)
	if err != nil {
		log.Printf("Failed to delete API key: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete API key"})
		return
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "API key deleted"})
}

func updateAPIKey(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	id := c.Param("id")
	idInt, err := strconv.Atoi(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key ID"})
		return
	}

	var req struct {
		IsActive *bool `json:"is_active"`
		Name     *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}
	argPos := 1

	if req.IsActive != nil {
		updates = append(updates, fmt.Sprintf("is_active = $%d", argPos))
		args = append(args, *req.IsActive)
		argPos++
	}
	if req.Name != nil {
		updates = append(updates, fmt.Sprintf("name = $%d", argPos))
		args = append(args, *req.Name)
		argPos++
	}
	if req.Description != nil {
		updates = append(updates, fmt.Sprintf("description = $%d", argPos))
		args = append(args, *req.Description)
		argPos++
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	args = append(args, idInt)
	query := fmt.Sprintf("UPDATE api_keys SET %s WHERE id = $%d", strings.Join(updates, ", "), argPos)

	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		log.Printf("Failed to update API key: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update API key"})
		return
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "API key updated"})
}

func extractFilename(meta map[string]string) string {
	if meta == nil {
		return "unknown.bin"
	}
	
	val, ok := meta["filename"]
	if !ok {
		val, ok = meta["name"]
	}
	if !ok {
		return "unknown.bin"
	}

	// Try to decode base64
	if decoded, err := base64.StdEncoding.DecodeString(val); err == nil {
		// Basic check if result looks like utf8 text
		return string(decoded)
	}
	
	// If not valid base64, assume raw
	return val
}

func metadataText(meta map[string]string, key string) string {
	if meta == nil {
		return ""
	}

	value := strings.TrimSpace(meta[key])
	if value == "" {
		return ""
	}

	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		decodedValue := strings.TrimSpace(string(decoded))
		if decodedValue != "" {
			return decodedValue
		}
	}

	return value
}

func isBusinessMultipartChunk(meta map[string]string) bool {
	return strings.EqualFold(metadataText(meta, "filecodebox_multipart"), "true") ||
		strings.EqualFold(metadataText(meta, "multipart_upload"), "true")
}

func isBusinessMultipartUpload(ctx context.Context, uploadID string) bool {
	if dataStore == nil || uploadID == "" {
		return false
	}

	upload, err := dataStore.GetUpload(ctx, uploadID)
	if err != nil {
		return false
	}

	info, err := upload.GetInfo(ctx)
	if err != nil {
		return false
	}

	return isBusinessMultipartChunk(info.MetaData)
}

func cleanupBusinessMultipartChunks(uploadIDs []string) {
	if len(uploadIDs) == 0 || dataStore == nil {
		return
	}

	ids := append([]string(nil), uploadIDs...)
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		terminater, ok := dataStore.(handler.TerminaterDataStore)
		if !ok {
			log.Printf("Business multipart chunk cleanup skipped: data store does not support termination")
			return
		}

		for _, uploadID := range ids {
			uploadID = normalizeUploadID(uploadID)
			if uploadID == "" {
				continue
			}

			upload, err := dataStore.GetUpload(bgCtx, uploadID)
			if err != nil {
				log.Printf("Failed to load business chunk for cleanup [%s]: %v", uploadID, err)
				continue
			}

			terminatableUpload := terminater.AsTerminatableUpload(upload)
			if terminatableUpload == nil {
				log.Printf("Business chunk is not terminatable [%s]", uploadID)
				continue
			}

			if err := terminatableUpload.Terminate(bgCtx); err != nil {
				log.Printf("Failed to cleanup business chunk [%s]: %v", uploadID, err)
			}
		}
	}()
}

func abortS3MultipartUpload(objectKey string, multipartUploadID string) {
	if s3Client == nil || objectKey == "" || multipartUploadID == "" {
		return
	}

	abortCtx, abortCancel := context.WithTimeout(context.Background(), time.Minute)
	defer abortCancel()
	_, abortErr := s3Client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s3Bucket),
		Key:      aws.String(objectKey),
		UploadId: aws.String(multipartUploadID),
	})
	if abortErr != nil {
		log.Printf("Failed to abort multipart upload %s: %v", objectKey, abortErr)
	}
}

func recoverInterruptedMergeTasks() {
	if db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		SELECT task_id, upload_id, filename, owner_key_hash, chunk_upload_ids, s3_multipart_upload_id, created_at, updated_at
		FROM merge_tasks
		WHERE status=$1
	`, mergeTaskStatusProcessing)
	if err != nil {
		log.Printf("Failed to load interrupted merge tasks: %v", err)
		return
	}
	defer rows.Close()

	recovered := 0
	for rows.Next() {
		var ownerHash sql.NullString
		var chunkUploadIDs string
		var s3MultipartUploadID sql.NullString
		task := &MergeTask{
			Status: mergeTaskStatusFailed,
			Error:  "merge task interrupted by server restart; source chunks were scheduled for cleanup, please upload again",
		}
		if err := rows.Scan(
			&task.TaskID,
			&task.UploadID,
			&task.Filename,
			&ownerHash,
			&chunkUploadIDs,
			&s3MultipartUploadID,
			&task.CreatedAt,
			&task.UpdatedAt,
		); err != nil {
			log.Printf("Failed to scan interrupted merge task: %v", err)
			continue
		}
		if ownerHash.Valid {
			task.OwnerHash = ownerHash.String
		}
		if s3MultipartUploadID.Valid {
			task.S3MultipartUploadID = s3MultipartUploadID.String
		}
		task.ChunkUploadIDs = decodeMergeTaskUploadIDs(chunkUploadIDs)
		task.UpdatedAt = time.Now().UTC()

		if task.S3MultipartUploadID != "" {
			abortS3MultipartUpload(task.UploadID, task.S3MultipartUploadID)
		}
		cleanupBusinessMultipartChunks(task.ChunkUploadIDs)
		persistMergeTask(task)
		recovered += 1
	}
	if err := rows.Err(); err != nil {
		log.Printf("Failed while reading interrupted merge tasks: %v", err)
	}
	if recovered > 0 {
		log.Printf("Recovered %d interrupted merge task(s)", recovered)
	}
}

func escapedS3CopySource(bucket string, key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}

	return url.PathEscape(bucket) + "/" + strings.Join(segments, "/")
}

func createShareCodeForUpload(ctx context.Context, uploadID string, filename string, ownerHash string) string {
	if db == nil {
		return uploadID
	}

	for i := 0; i < 5; i++ {
		code := genCode(8)
		exp := shareCodeExpiresAt()
		res, err := db.ExecContext(ctx,
			"INSERT INTO share_codes(code, upload_id, filename, owner_key_hash, expires_at, max_downloads, downloads) VALUES($1,$2,$3,$4,$5,$6,0) ON CONFLICT (code) DO NOTHING",
			code, uploadID, filename, nullableString(ownerHash), exp, maxDownloads)
		if err == nil {
			if rows, _ := res.RowsAffected(); rows > 0 {
				return code
			}
		}
	}

	exp := shareCodeExpiresAt()
	_, _ = db.ExecContext(ctx, `
		INSERT INTO share_codes(code, upload_id, filename, owner_key_hash, expires_at, max_downloads, downloads)
		VALUES($1,$2,$3,$4,$5,$6,0)
		ON CONFLICT (code) DO UPDATE SET
			filename = EXCLUDED.filename,
			owner_key_hash = COALESCE(share_codes.owner_key_hash, EXCLUDED.owner_key_hash)
	`, uploadID, uploadID, filename, nullableString(ownerHash), exp, maxDownloads)
	return uploadID
}

func newMergeTask(taskID string, filename string, ownerHash string) *MergeTask {
	now := time.Now().UTC()
	return &MergeTask{
		TaskID:    taskID,
		Status:    mergeTaskStatusProcessing,
		UploadID:  taskID,
		Filename:  filename,
		OwnerHash: ownerHash,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func mergeTaskTimeout(chunkCount int) time.Duration {
	timeout := mergeTaskBaseTimeout + time.Duration(chunkCount)*mergeTaskPerChunkTimeout
	if timeout < mergeTaskMinTimeout {
		return mergeTaskMinTimeout
	}
	return timeout
}

func encodeMergeTaskUploadIDs(uploadIDs []string) string {
	if len(uploadIDs) == 0 {
		return ""
	}
	cleanIDs := make([]string, 0, len(uploadIDs))
	for _, uploadID := range uploadIDs {
		uploadID = normalizeUploadID(strings.TrimSpace(uploadID))
		if uploadID != "" {
			cleanIDs = append(cleanIDs, uploadID)
		}
	}
	return strings.Join(cleanIDs, ",")
}

func decodeMergeTaskUploadIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	uploadIDs := make([]string, 0, len(parts))
	for _, part := range parts {
		uploadID := normalizeUploadID(strings.TrimSpace(part))
		if uploadID != "" {
			uploadIDs = append(uploadIDs, uploadID)
		}
	}
	return uploadIDs
}

func persistMergeTask(task *MergeTask) {
	if db == nil || task == nil {
		return
	}

	task.mu.RLock()
	taskID := task.TaskID
	uploadID := task.UploadID
	filename := task.Filename
	ownerHash := task.OwnerHash
	status := task.Status
	code := task.Code
	errMessage := task.Error
	chunkUploadIDs := encodeMergeTaskUploadIDs(task.ChunkUploadIDs)
	s3MultipartUploadID := task.S3MultipartUploadID
	createdAt := task.CreatedAt
	updatedAt := task.UpdatedAt
	task.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, `
		INSERT INTO merge_tasks(task_id, upload_id, filename, owner_key_hash, status, code, error, chunk_upload_ids, s3_multipart_upload_id, created_at, updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (task_id) DO UPDATE SET
			upload_id = EXCLUDED.upload_id,
			filename = EXCLUDED.filename,
			owner_key_hash = EXCLUDED.owner_key_hash,
			status = EXCLUDED.status,
			code = EXCLUDED.code,
			error = EXCLUDED.error,
			chunk_upload_ids = EXCLUDED.chunk_upload_ids,
			s3_multipart_upload_id = EXCLUDED.s3_multipart_upload_id,
			updated_at = EXCLUDED.updated_at
	`, taskID, uploadID, filename, nullableString(ownerHash), status, nullableString(code), nullableString(errMessage), chunkUploadIDs, nullableString(s3MultipartUploadID), createdAt, updatedAt)
	if err != nil {
		log.Printf("Failed to persist merge task %s: %v", taskID, err)
	}
}

func loadPersistedMergeTask(taskID string) (*MergeTask, bool) {
	if db == nil || taskID == "" {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var ownerHash sql.NullString
	var code sql.NullString
	var errMessage sql.NullString
	var chunkUploadIDs string
	var s3MultipartUploadID sql.NullString
	task := &MergeTask{}
	err := db.QueryRowContext(ctx, `
		SELECT task_id, upload_id, filename, owner_key_hash, status, code, error, chunk_upload_ids, s3_multipart_upload_id, created_at, updated_at
		FROM merge_tasks
		WHERE task_id=$1
	`, taskID).Scan(
		&task.TaskID,
		&task.UploadID,
		&task.Filename,
		&ownerHash,
		&task.Status,
		&code,
		&errMessage,
		&chunkUploadIDs,
		&s3MultipartUploadID,
		&task.CreatedAt,
		&task.UpdatedAt,
	)
	if err != nil {
		return nil, false
	}

	if ownerHash.Valid {
		task.OwnerHash = ownerHash.String
	}
	if code.Valid {
		task.Code = code.String
	}
	if errMessage.Valid {
		task.Error = errMessage.String
	}
	if s3MultipartUploadID.Valid {
		task.S3MultipartUploadID = s3MultipartUploadID.String
	}
	task.ChunkUploadIDs = decodeMergeTaskUploadIDs(chunkUploadIDs)

	return task, true
}

func (task *MergeTask) status() string {
	task.mu.RLock()
	defer task.mu.RUnlock()
	return task.Status
}

func (task *MergeTask) scheduleCleanup() {
	time.AfterFunc(mergeTaskCleanupDelay, func() {
		value, exists := mergeTasks.Load(task.TaskID)
		if !exists {
			return
		}
		if currentTask, ok := value.(*MergeTask); ok && currentTask == task {
			mergeTasks.Delete(task.TaskID)
		}
	})
}

func (task *MergeTask) markSuccess(code string) {
	task.mu.Lock()
	task.Status = mergeTaskStatusSuccess
	task.Code = code
	task.Error = ""
	task.UpdatedAt = time.Now().UTC()
	task.mu.Unlock()

	persistMergeTask(task)
	task.scheduleCleanup()
}

func (task *MergeTask) markFailed(err error) {
	message := "merge task failed"
	if err != nil {
		message = err.Error()
	}

	task.mu.Lock()
	task.Status = mergeTaskStatusFailed
	task.Error = message
	task.UpdatedAt = time.Now().UTC()
	task.mu.Unlock()

	persistMergeTask(task)
	task.scheduleCleanup()
}

func (task *MergeTask) setOwnerHash(ownerHash string) {
	if ownerHash == "" {
		return
	}

	task.mu.Lock()
	if task.OwnerHash == "" {
		task.OwnerHash = ownerHash
	}
	task.UpdatedAt = time.Now().UTC()
	task.mu.Unlock()

	persistMergeTask(task)
}

func (task *MergeTask) setS3MultipartUploadID(uploadID string) {
	if uploadID == "" {
		return
	}

	task.mu.Lock()
	task.S3MultipartUploadID = uploadID
	task.UpdatedAt = time.Now().UTC()
	task.mu.Unlock()

	persistMergeTask(task)
}

func mergeTaskResponse(task *MergeTask) gin.H {
	task.mu.RLock()
	taskID := task.TaskID
	status := task.Status
	uploadID := task.UploadID
	code := task.Code
	filename := task.Filename
	errMessage := task.Error
	createdAt := task.CreatedAt
	updatedAt := task.UpdatedAt
	task.mu.RUnlock()

	response := gin.H{
		"task_id":    taskID,
		"status":     status,
		"upload_id":  uploadID,
		"filename":   filename,
		"created_at": createdAt.Format(time.RFC3339),
		"updated_at": updatedAt.Format(time.RFC3339),
	}
	if errMessage != "" {
		response["error"] = errMessage
	}
	if status == mergeTaskStatusSuccess && code != "" {
		for key, value := range shareCodeResponse(uploadID, code, filename) {
			response[key] = value
		}
	}

	return response
}

func currentRequestOwner(c *gin.Context) (string, bool) {
	ownerHash := ""
	if value, exists := c.Get("api_key_hash"); exists {
		if hash, ok := value.(string); ok {
			ownerHash = hash
		}
	}

	isAdmin := false
	if value, exists := c.Get("is_admin"); exists {
		if admin, ok := value.(bool); ok {
			isAdmin = admin
		}
	}

	return ownerHash, isAdmin
}

func canAccessMergeTask(c *gin.Context, task *MergeTask) bool {
	ownerHash, isAdmin := currentRequestOwner(c)
	if isAdmin || task == nil {
		return true
	}

	task.mu.RLock()
	taskOwnerHash := task.OwnerHash
	task.mu.RUnlock()

	return taskOwnerHash == "" || ownerHash == "" || taskOwnerHash == ownerHash
}

func prepareFinalizeMultipartRequest(req *FinalizeMultipartRequest) ([]FinalizeMultipartChunk, []string, error) {
	req.Filename = strings.TrimSpace(req.Filename)
	req.Filetype = strings.TrimSpace(req.Filetype)
	if req.Filename == "" {
		return nil, nil, fmt.Errorf("filename is required")
	}
	if req.TotalSize <= 0 {
		return nil, nil, fmt.Errorf("total_size must be greater than zero")
	}
	if len(req.Chunks) == 0 {
		return nil, nil, fmt.Errorf("chunks are required")
	}
	if len(req.Chunks) > maxS3MultipartParts {
		return nil, nil, fmt.Errorf("too many chunks for S3 multipart upload")
	}

	chunks := append([]FinalizeMultipartChunk(nil), req.Chunks...)
	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].Index == chunks[j].Index {
			return chunks[i].Start < chunks[j].Start
		}
		return chunks[i].Index < chunks[j].Index
	})

	seenUploadIDs := make(map[string]bool, len(chunks))
	expectedStart := int64(0)
	uploadIDs := make([]string, 0, len(chunks))
	for i := range chunks {
		chunks[i].UploadID = normalizeUploadID(strings.TrimSpace(chunks[i].UploadID))
		if chunks[i].UploadID == "" || !codeRegex.MatchString(chunks[i].UploadID) {
			return nil, nil, fmt.Errorf("invalid chunk upload_id at index %d", i)
		}
		if seenUploadIDs[chunks[i].UploadID] {
			return nil, nil, fmt.Errorf("duplicated chunk upload_id at index %d", i)
		}
		seenUploadIDs[chunks[i].UploadID] = true

		if chunks[i].End == 0 && chunks[i].Size > 0 {
			chunks[i].End = chunks[i].Start + chunks[i].Size
		}
		if chunks[i].Size == 0 {
			chunks[i].Size = chunks[i].End - chunks[i].Start
		}
		if chunks[i].Start != expectedStart || chunks[i].End <= chunks[i].Start || chunks[i].Size != chunks[i].End-chunks[i].Start {
			return nil, nil, fmt.Errorf("invalid chunk byte range at index %d", i)
		}
		if chunks[i].Size > maxS3MultipartPartSize {
			return nil, nil, fmt.Errorf("chunk %d exceeds the 5GB S3 multipart part limit", i)
		}

		expectedStart = chunks[i].End
		uploadIDs = append(uploadIDs, chunks[i].UploadID)
	}
	if expectedStart != req.TotalSize {
		return nil, nil, fmt.Errorf("chunks do not cover total_size")
	}

	return chunks, uploadIDs, nil
}

func mergeTaskID(req FinalizeMultipartRequest, chunks []FinalizeMultipartChunk) string {
	hash := sha256.New()
	hash.Write([]byte(req.Filename))
	hash.Write([]byte{0})
	hash.Write([]byte(req.Filetype))
	hash.Write([]byte{0})
	hash.Write([]byte(strconv.FormatInt(req.TotalSize, 10)))
	for _, chunk := range chunks {
		hash.Write([]byte{0})
		hash.Write([]byte(chunk.UploadID))
		hash.Write([]byte{0})
		hash.Write([]byte(strconv.FormatInt(chunk.Start, 10)))
		hash.Write([]byte{0})
		hash.Write([]byte(strconv.FormatInt(chunk.End, 10)))
	}

	return "final-" + hex.EncodeToString(hash.Sum(nil))[:32]
}

func completedMergeTaskResponse(c *gin.Context, taskID string) (gin.H, int, bool) {
	if db == nil {
		return nil, http.StatusNotFound, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var code string
	var filename string
	var ownerHash sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT code, filename, owner_key_hash
		FROM share_codes
		WHERE upload_id=$1
		ORDER BY created_at DESC
		LIMIT 1
	`, taskID).Scan(&code, &filename, &ownerHash)
	if err != nil {
		return nil, http.StatusNotFound, false
	}

	requestOwnerHash, isAdmin := currentRequestOwner(c)
	if !isAdmin && ownerHash.Valid && requestOwnerHash != "" && ownerHash.String != requestOwnerHash {
		return gin.H{"error": "merge task ownership mismatch"}, http.StatusForbidden, true
	}

	response := shareCodeResponse(taskID, code, filename)
	response["task_id"] = taskID
	response["status"] = mergeTaskStatusSuccess
	response["upload_id"] = taskID
	return response, http.StatusOK, true
}

func finalizeMultipartUpload(c *gin.Context) {
	if s3Client == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "S3 storage is required for multipart finalize"})
		return
	}

	var req FinalizeMultipartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid finalize request"})
		return
	}

	chunks, uploadIDs, validationErr := prepareFinalizeMultipartRequest(&req)
	if validationErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": validationErr.Error()})
		return
	}

	ownerHash, isAdmin := currentRequestOwner(c)
	taskID := mergeTaskID(req, chunks)
	if response, status, exists := completedMergeTaskResponse(c, taskID); exists {
		if status == http.StatusOK {
			response["message"] = "merge task already completed"
		}
		c.JSON(status, response)
		return
	}

	task := newMergeTask(taskID, req.Filename, ownerHash)
	task.ChunkUploadIDs = append([]string(nil), uploadIDs...)
	actual, loaded := mergeTasks.LoadOrStore(taskID, task)
	if loaded {
		existingTask := actual.(*MergeTask)
		if !canAccessMergeTask(c, existingTask) {
			c.JSON(http.StatusForbidden, gin.H{"error": "merge task ownership mismatch"})
			return
		}
		if existingTask.status() == mergeTaskStatusFailed {
			mergeTasks.Delete(taskID)
			actual, loaded = mergeTasks.LoadOrStore(taskID, task)
			if loaded {
				existingTask = actual.(*MergeTask)
				response := mergeTaskResponse(existingTask)
				response["message"] = "merge task already exists"
				c.JSON(http.StatusOK, response)
				return
			}
		} else {
			response := mergeTaskResponse(existingTask)
			response["message"] = "merge task already exists"
			c.JSON(http.StatusOK, response)
			return
		}
	}

	persistMergeTask(task)
	go runFinalizeMultipartTask(task, req, chunks, uploadIDs, ownerHash, isAdmin)

	response := mergeTaskResponse(task)
	response["message"] = "merge task submitted"
	c.JSON(http.StatusOK, response)
}

func runFinalizeMultipartTask(task *MergeTask, req FinalizeMultipartRequest, chunks []FinalizeMultipartChunk, uploadIDs []string, ownerHash string, isAdmin bool) {
	ctx, cancel := context.WithTimeout(context.Background(), mergeTaskTimeout(len(chunks)))
	defer cancel()

	cleanupOriginalChunksOnExit := false
	mergeSucceeded := false
	sourceChunksCleaned := false
	cleanupSourceChunks := func() {
		if sourceChunksCleaned {
			return
		}
		sourceChunksCleaned = true
		cleanupBusinessMultipartChunks(uploadIDs)
	}
	defer func() {
		if cleanupOriginalChunksOnExit && !mergeSucceeded {
			cleanupSourceChunks()
		}
	}()

	sourceKeys := make([]string, len(chunks))
	for i, chunk := range chunks {
		upload, err := dataStore.GetUpload(ctx, chunk.UploadID)
		if err != nil {
			task.markFailed(fmt.Errorf("chunk %d is not available: %w", i, err))
			return
		}

		info, err := upload.GetInfo(ctx)
		if err != nil {
			task.markFailed(fmt.Errorf("chunk %d metadata is not available: %w", i, err))
			return
		}
		if info.IsPartial || info.IsFinal || info.Offset < info.Size {
			task.markFailed(fmt.Errorf("chunk %d is not completed", i))
			return
		}
		if info.Size != chunk.Size {
			task.markFailed(fmt.Errorf("chunk %d size mismatch", i))
			return
		}

		if db != nil && ownerHash != "" && isAdmin != true {
			chunkOwnerHash := ownerHashForUpload(ctx, chunk.UploadID)
			if chunkOwnerHash != "" && chunkOwnerHash != ownerHash {
				task.markFailed(fmt.Errorf("chunk ownership mismatch"))
				return
			}
		}

		key, err := resolveS3ObjectKey(ctx, chunk.UploadID)
		if err != nil {
			task.markFailed(fmt.Errorf("chunk %d object is not available: %w", i, err))
			return
		}
		sourceKeys[i] = key
	}
	cleanupOriginalChunksOnExit = true

	if ownerHash == "" && len(uploadIDs) > 0 {
		ownerHash = ownerHashForUpload(ctx, uploadIDs[0])
		task.setOwnerHash(ownerHash)
	}

	contentType := strings.TrimSpace(req.Filetype)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	createOut, err := s3Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s3Bucket),
		Key:         aws.String(task.UploadID),
		ContentType: aws.String(contentType),
		Metadata: map[string]string{
			"filecodebox-finalized": "true",
		},
	})
	if err != nil {
		log.Printf("Failed to create multipart upload for %s: %v", task.UploadID, err)
		task.markFailed(fmt.Errorf("failed to start S3 multipart compose: %w", err))
		return
	}
	if createOut.UploadId == nil || *createOut.UploadId == "" {
		task.markFailed(fmt.Errorf("S3 multipart upload id is empty"))
		return
	}
	s3MultipartUploadID := *createOut.UploadId
	task.setS3MultipartUploadID(s3MultipartUploadID)

	completedParts := make([]types.CompletedPart, len(sourceKeys))
	copyErrCh := make(chan error, len(sourceKeys))
	copyLimiter := make(chan struct{}, maxS3PartCopyConcurrency)
	var copyWg sync.WaitGroup
	var copyMu sync.Mutex

	for i, key := range sourceKeys {
		copyWg.Add(1)
		go func(index int, sourceKey string) {
			defer copyWg.Done()

			copyLimiter <- struct{}{}
			defer func() { <-copyLimiter }()

			partNumber := int32(index + 1)
			copyOut, err := s3Client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
				Bucket:     aws.String(s3Bucket),
				Key:        aws.String(task.UploadID),
				UploadId:   aws.String(s3MultipartUploadID),
				PartNumber: aws.Int32(partNumber),
				CopySource: aws.String(escapedS3CopySource(s3Bucket, sourceKey)),
			})
			if err != nil {
				copyErrCh <- fmt.Errorf("chunk %d copy failed: %w", index, err)
				return
			}
			if copyOut.CopyPartResult == nil || copyOut.CopyPartResult.ETag == nil || *copyOut.CopyPartResult.ETag == "" {
				copyErrCh <- fmt.Errorf("chunk %d copy result is missing ETag", index)
				return
			}

			copyMu.Lock()
			completedParts[index] = types.CompletedPart{
				ETag:       copyOut.CopyPartResult.ETag,
				PartNumber: aws.Int32(partNumber),
			}
			copyMu.Unlock()
		}(i, key)
	}

	copyWg.Wait()
	close(copyErrCh)
	if err := <-copyErrCh; err != nil {
		abortS3MultipartUpload(task.UploadID, s3MultipartUploadID)
		log.Printf("Failed to copy chunks into %s: %v", task.UploadID, err)
		task.markFailed(err)
		return
	}

	_, err = s3Client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s3Bucket),
		Key:      aws.String(task.UploadID),
		UploadId: aws.String(s3MultipartUploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})
	if err != nil {
		abortS3MultipartUpload(task.UploadID, s3MultipartUploadID)
		log.Printf("Failed to complete multipart upload for %s: %v", task.UploadID, err)
		task.markFailed(fmt.Errorf("failed to complete S3 multipart compose: %w", err))
		return
	}

	mu.Lock()
	codeToUpload[task.UploadID] = UploadRecord{UploadID: task.UploadID, Filename: req.Filename}
	mu.Unlock()
	cacheS3ObjectKey(task.UploadID, task.UploadID)
	aggregateUploadMetrics(task.UploadID, uploadIDs)
	mergeSucceeded = true
	cleanupSourceChunks()
	if ownerHash != "" {
		rememberUploadOwner(task.UploadID, ownerHash)
	}
	code := createShareCodeForUpload(ctx, task.UploadID, req.Filename, ownerHash)
	task.markSuccess(code)
}

func getMergeStatus(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("task_id"))
	if taskID == "" || !codeRegex.MatchString(taskID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merge task not found"})
		return
	}

	if value, exists := mergeTasks.Load(taskID); exists {
		task := value.(*MergeTask)
		if !canAccessMergeTask(c, task) {
			c.JSON(http.StatusForbidden, gin.H{"error": "merge task ownership mismatch"})
			return
		}
		c.JSON(http.StatusOK, mergeTaskResponse(task))
		return
	}

	if task, exists := loadPersistedMergeTask(taskID); exists {
		if !canAccessMergeTask(c, task) {
			c.JSON(http.StatusForbidden, gin.H{"error": "merge task ownership mismatch"})
			return
		}
		c.JSON(http.StatusOK, mergeTaskResponse(task))
		return
	}

	if response, status, exists := completedMergeTaskResponse(c, taskID); exists {
		c.JSON(status, response)
		return
	}

	c.JSON(http.StatusNotFound, gin.H{"error": "merge task not found"})
}

func getShareCode(c *gin.Context) {
    var req struct {
        UploadID string `json:"upload_id" binding:"required"`
    }
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(400, gin.H{"error": "upload_id is required"})
        return
    }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if isBusinessMultipartUpload(ctx, req.UploadID) {
		cancel()
		c.JSON(400, gin.H{"error": "Business multipart chunks cannot be shared directly"})
		return
	}
	cancel()

	refreshCompletedUploadRuntimeInfo(req.UploadID)

    if db != nil {
        var code string
        var filename string
        var expires sql.NullTime
        var downloads int
        var maxd int
        
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        ownerHash := ownerHashForUpload(ctx, req.UploadID)

        err := db.QueryRowContext(ctx, "SELECT code, filename, expires_at, downloads, max_downloads FROM share_codes WHERE upload_id=$1 ORDER BY created_at DESC LIMIT 1", req.UploadID).Scan(&code, &filename, &expires, &downloads, &maxd)
        if err == nil {
            // Upgrade legacy long code (where code == uploadID) to short code
            // Only if code equals uploadID (meaning it was auto-inserted as fallback or legacy)
            if code != req.UploadID {
                 if (!expires.Valid || time.Now().UTC().Before(expires.Time)) && (maxd == 0 || downloads < maxd) {
                    c.JSON(200, shareCodeResponse(req.UploadID, code, filename))
                    return
                 }
            }
        }
        
        // Conflict Retry Logic
        var newCode string
        for i := 0; i < 5; i++ {
             newCode = genCode(8)
             exp := shareCodeExpiresAt()
              res, ierr := db.ExecContext(ctx, "INSERT INTO share_codes(code, upload_id, filename, owner_key_hash, expires_at, max_downloads, downloads) VALUES($1,$2,$3,$4,$5,$6,0) ON CONFLICT (code) DO NOTHING", newCode, req.UploadID, filenameOrCache(req.UploadID), nullableString(ownerHash), exp, maxDownloads)
              if ierr == nil {
                  if rows, _ := res.RowsAffected(); rows > 0 {
                      c.JSON(200, shareCodeResponse(req.UploadID, newCode, filenameOrCache(req.UploadID)))
                      return
                  }
              }
        }
        
        // Fallback to UploadID as code
        c.JSON(200, shareCodeResponse(req.UploadID, req.UploadID, filenameOrCache(req.UploadID)))
        return
    }

    // 1. Check Cache
    mu.RLock()
    record, exists := codeToUpload[req.UploadID]
    mu.RUnlock()

	// 2. Fallback: Check Store Directly (Fixes race condition & restart data loss)
	if !exists {
		log.Printf("UploadID %s not in cache, checking store...", req.UploadID)
		upload, err := dataStore.GetUpload(context.Background(), req.UploadID)
		if err == nil {
			info, err := upload.GetInfo(context.Background())
			if err == nil {
				filename := extractFilename(info.MetaData)
				record = UploadRecord{
					UploadID: req.UploadID,
					Filename: filename,
				}
				// Update Cache
				mu.Lock()
				codeToUpload[req.UploadID] = record
				mu.Unlock()
				exists = true
				log.Printf("Found in store: %s (%s)", req.UploadID, filename)
			} else {
                log.Printf("Failed to get info for upload %s: %v", req.UploadID, err)
            }
		} else {
            log.Printf("Failed to get upload %s from store: %v", req.UploadID, err)
        }
	}

    if !exists {
        c.JSON(404, gin.H{"error": "Upload not found or not completed"})
        return
    }

    if db != nil {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        
        exp := shareCodeExpiresAt()
        _, _ = db.ExecContext(ctx, "INSERT INTO share_codes(code, upload_id, filename, expires_at, max_downloads) VALUES($1,$2,$3,$4,$5) ON CONFLICT (code) DO UPDATE SET filename=EXCLUDED.filename", req.UploadID, req.UploadID, record.Filename, exp, maxDownloads)
    }

    c.JSON(200, shareCodeResponse(req.UploadID, req.UploadID, record.Filename))
}

func retrieveFile(c *gin.Context) {
    code := c.Param("code")

    // Validate code
    if !codeRegex.MatchString(code) {
        c.JSON(404, gin.H{"error": "invalid code"})
        return
    }

    if db != nil {
        var uploadID string
        var filename string
        var expires sql.NullTime
        var downloads int
        var maxd int

        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        err := db.QueryRowContext(ctx, "SELECT upload_id, filename, expires_at, downloads, max_downloads FROM share_codes WHERE code=$1", code).Scan(&uploadID, &filename, &expires, &downloads, &maxd)
        if err == nil {
            if expires.Valid && time.Now().UTC().After(expires.Time) {
                c.JSON(410, gin.H{"error": "Code expired"})
                return
            }
            if maxd > 0 && downloads >= maxd {
                c.JSON(429, gin.H{"error": "Download limit reached"})
                return
            }
            c.JSON(200, gin.H{"upload_id": uploadID, "filename": filename, "url": "/files/" + code})
            return
        }
        // Not found in DB: treat code as uploadID ONLY if the upload actually exists,
        // then auto-register mapping safely without overriding existing short codes.
        upload, err2 := dataStore.GetUpload(context.Background(), code)
        if err2 != nil {
            c.JSON(404, gin.H{"error": "Code not found"})
            return
        }
        info, err3 := upload.GetInfo(context.Background())
        if err3 != nil {
            c.JSON(404, gin.H{"error": "Code not found"})
            return
        }
		if isBusinessMultipartChunk(info.MetaData) {
			c.JSON(404, gin.H{"error": "Code not found"})
			return
		}
        resolved := extractFilename(info.MetaData)
        exp := shareCodeExpiresAt()
        // Insert mapping and avoid hijacking existing short codes by only updating
        // when the existing row already points to the same upload_id.
        _, _ = db.ExecContext(ctx, `
            INSERT INTO share_codes(code, upload_id, filename, expires_at, max_downloads, downloads)
            VALUES($1,$2,$3,$4,$5,0)
            ON CONFLICT (code) DO UPDATE SET filename=EXCLUDED.filename
            WHERE share_codes.upload_id = EXCLUDED.upload_id
        `, code, code, resolved, exp, maxDownloads)
        c.JSON(200, gin.H{"upload_id": code, "filename": resolved, "url": "/files/" + code})
        return
    }

    mu.RLock()
    record, exists := codeToUpload[code]
    mu.RUnlock()

    // Fallback lookup
    if !exists {
        if upload, err := dataStore.GetUpload(context.Background(), code); err == nil {
            if info, err := upload.GetInfo(context.Background()); err == nil {
				if isBusinessMultipartChunk(info.MetaData) {
					c.JSON(404, gin.H{"error": "Code not found"})
					return
				}
                record = UploadRecord{
                    UploadID: code,
                    Filename: extractFilename(info.MetaData),
                }
                exists = true
            }
        }
    }

	if !exists {
		c.JSON(404, gin.H{"error": "Code not found"})
		return
	}

	c.JSON(200, gin.H{
		"upload_id": record.UploadID,
		"filename":  record.Filename,
		"url":       func() string { if db != nil { return "/files/" + code } else { return "/files/" + record.UploadID } }(),
	})
}

func genCode(n int) string {
    const chars = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
    b := make([]byte, n)
    for i := 0; i < n; i++ {
        x, _ := crand.Int(crand.Reader, big.NewInt(int64(len(chars))))
        b[i] = chars[x.Int64()]
    }
    return string(b)
}

func filenameOrCache(uploadID string) string {
    mu.RLock()
    r, ok := codeToUpload[uploadID]
    mu.RUnlock()
    if ok { return r.Filename }
    if upload, err := dataStore.GetUpload(context.Background(), uploadID); err == nil {
        if info, err := upload.GetInfo(context.Background()); err == nil {
            return extractFilename(info.MetaData)
        }
    }
    return "unknown.bin"
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func parseIntHeader(value string) int64 {
	if value == "" {
		return 0
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}

	return parsed
}

func recordUploadMetric(uploadID string, bytes int64, startedAt time.Time, finishedAt time.Time) {
	if uploadID == "" || bytes <= 0 || !finishedAt.After(startedAt) {
		return
	}

	metricsMu.Lock()
	metric := uploadMetrics[uploadID]
	metric.Bytes += bytes
	metric.Intervals = append(metric.Intervals, UploadMetricInterval{Start: startedAt, End: finishedAt})
	uploadMetrics[uploadID] = metric
	metricsMu.Unlock()

	persistUploadMetricInterval(uploadID, bytes, startedAt, finishedAt)
}

func aggregateUploadMetrics(finalUploadID string, partialUploadIDs []string) {
	if finalUploadID == "" || len(partialUploadIDs) == 0 {
		return
	}

	metricsMu.Lock()
	finalMetric := uploadMetrics[finalUploadID]
	for _, partialUploadID := range partialUploadIDs {
		partialUploadID = normalizeUploadID(partialUploadID)
		if partialUploadID == "" || partialUploadID == finalUploadID {
			continue
		}

		partialMetric, exists := uploadMetrics[partialUploadID]
		if !exists {
			continue
		}

		finalMetric.Bytes += partialMetric.Bytes
		finalMetric.Intervals = append(finalMetric.Intervals, partialMetric.Intervals...)
		delete(uploadMetrics, partialUploadID)
	}
	uploadMetrics[finalUploadID] = finalMetric
	metricsMu.Unlock()

	persistFinalUploadMetric(finalUploadID, partialUploadIDs, finalMetric)
}

func clearUploadRuntimeState(uploadID string) {
	if uploadID == "" {
		return
	}

	metricsMu.Lock()
	delete(uploadMetrics, uploadID)
	metricsMu.Unlock()

	mu.Lock()
	delete(codeToUpload, uploadID)
	delete(s3KeyCache, uploadID)
	mu.Unlock()
}

func getUploadMetric(uploadID string) (UploadMetric, bool) {
	if uploadID == "" {
		return UploadMetric{}, false
	}

	metricsMu.RLock()
	metric, exists := uploadMetrics[uploadID]
	if exists {
		metric.Intervals = append([]UploadMetricInterval(nil), metric.Intervals...)
	}
	metricsMu.RUnlock()

	return metric, exists && metric.Bytes > 0 && uploadMetricDuration(metric) > 0
}

func refreshCompletedUploadRuntimeInfo(uploadID string) {
	if uploadID == "" || dataStore == nil {
		return
	}

	upload, err := dataStore.GetUpload(context.Background(), uploadID)
	if err != nil {
		return
	}

	info, err := upload.GetInfo(context.Background())
	if err != nil || info.IsPartial {
		return
	}
	if isBusinessMultipartChunk(info.MetaData) {
		return
	}

	if info.IsFinal {
		aggregateUploadMetrics(uploadID, info.PartialUploads)
	}

	filename := extractFilename(info.MetaData)
	mu.Lock()
	if _, exists := codeToUpload[uploadID]; !exists {
		codeToUpload[uploadID] = UploadRecord{UploadID: uploadID, Filename: filename}
	}
	mu.Unlock()
}

func uploadMetricDuration(metric UploadMetric) time.Duration {
	if len(metric.Intervals) == 0 {
		return 0
	}

	intervals := append([]UploadMetricInterval(nil), metric.Intervals...)
	sort.Slice(intervals, func(i, j int) bool {
		return intervals[i].Start.Before(intervals[j].Start)
	})

	var total time.Duration
	currentStart := intervals[0].Start
	currentEnd := intervals[0].End

	for _, interval := range intervals[1:] {
		if interval.End.Before(interval.Start) || interval.End.Equal(interval.Start) {
			continue
		}

		if interval.Start.After(currentEnd) {
			total += currentEnd.Sub(currentStart)
			currentStart = interval.Start
			currentEnd = interval.End
			continue
		}

		if interval.End.After(currentEnd) {
			currentEnd = interval.End
		}
	}

	total += currentEnd.Sub(currentStart)
	return total
}

func summarizeUploadMetric(metric UploadMetric) (UploadMetricSummary, bool) {
	if metric.Bytes <= 0 || len(metric.Intervals) == 0 {
		return UploadMetricSummary{}, false
	}

	validIntervals := make([]UploadMetricInterval, 0, len(metric.Intervals))
	for _, interval := range metric.Intervals {
		if interval.End.After(interval.Start) {
			validIntervals = append(validIntervals, interval)
		}
	}
	if len(validIntervals) == 0 {
		return UploadMetricSummary{}, false
	}

	startedAt := validIntervals[0].Start
	finishedAt := validIntervals[0].End
	for _, interval := range validIntervals[1:] {
		if interval.Start.Before(startedAt) {
			startedAt = interval.Start
		}
		if interval.End.After(finishedAt) {
			finishedAt = interval.End
		}
	}

	metric.Intervals = validIntervals
	duration := uploadMetricDuration(metric)
	if duration <= 0 {
		return UploadMetricSummary{}, false
	}

	averageMbps := averageUploadMbps(metric.Bytes, duration)
	return UploadMetricSummary{
		Bytes:                metric.Bytes,
		StartedAt:            startedAt,
		FinishedAt:           finishedAt,
		Duration:             duration,
		AverageMbps:          averageMbps,
		BandwidthUtilization: uploadBandwidthUtilization(averageMbps),
		BandwidthBaselineMbps: bandwidthBaselineMbps,
	}, true
}

func averageUploadMbps(bytes int64, duration time.Duration) float64 {
	if bytes <= 0 || duration <= 0 {
		return 0
	}
	return float64(bytes) * 8 / duration.Seconds() / 1000 / 1000
}

func uploadBandwidthUtilization(averageMbps float64) float64 {
	if averageMbps <= 0 || bandwidthBaselineMbps <= 0 {
		return 0
	}
	return averageMbps / bandwidthBaselineMbps * 100
}

func uploadMetricResponse(summary UploadMetricSummary) gin.H {
	return gin.H{
		"bytes":                   summary.Bytes,
		"started_at":              summary.StartedAt.Format(time.RFC3339),
		"finished_at":             summary.FinishedAt.Format(time.RFC3339),
		"duration_ms":             float64(summary.Duration) / float64(time.Millisecond),
		"average_mbps":            summary.AverageMbps,
		"bandwidth_utilization":   summary.BandwidthUtilization,
		"bandwidth_baseline_mbps": summary.BandwidthBaselineMbps,
	}
}

func persistUploadMetricInterval(uploadID string, bytes int64, startedAt time.Time, finishedAt time.Time) {
	if db == nil || uploadID == "" || bytes <= 0 || !finishedAt.After(startedAt) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, `
		INSERT INTO upload_metric_intervals(upload_id, bytes, started_at, finished_at)
		VALUES($1, $2, $3, $4)
	`, uploadID, bytes, startedAt, finishedAt)
	if err != nil {
		log.Printf("Failed to persist upload metric interval for %s: %v", uploadID, err)
		return
	}

	duration := finishedAt.Sub(startedAt)
	averageMbps := averageUploadMbps(bytes, duration)
	durationMs := duration.Milliseconds()
	_, err = db.ExecContext(ctx, `
		INSERT INTO upload_metrics(upload_id, bytes, started_at, finished_at, duration_ms, average_mbps, bandwidth_utilization, bandwidth_baseline_mbps, updated_at)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (upload_id) DO UPDATE SET
			bytes = upload_metrics.bytes + EXCLUDED.bytes,
			started_at = LEAST(upload_metrics.started_at, EXCLUDED.started_at),
			finished_at = GREATEST(upload_metrics.finished_at, EXCLUDED.finished_at),
			duration_ms = GREATEST(0, (EXTRACT(EPOCH FROM (GREATEST(upload_metrics.finished_at, EXCLUDED.finished_at) - LEAST(upload_metrics.started_at, EXCLUDED.started_at))) * 1000)::BIGINT),
			average_mbps = CASE
				WHEN EXTRACT(EPOCH FROM (GREATEST(upload_metrics.finished_at, EXCLUDED.finished_at) - LEAST(upload_metrics.started_at, EXCLUDED.started_at))) > 0
				THEN ((upload_metrics.bytes + EXCLUDED.bytes)::DOUBLE PRECISION * 8 / EXTRACT(EPOCH FROM (GREATEST(upload_metrics.finished_at, EXCLUDED.finished_at) - LEAST(upload_metrics.started_at, EXCLUDED.started_at))) / 1000000)
				ELSE 0
			END,
			bandwidth_utilization = CASE
				WHEN EXCLUDED.bandwidth_baseline_mbps > 0 AND EXTRACT(EPOCH FROM (GREATEST(upload_metrics.finished_at, EXCLUDED.finished_at) - LEAST(upload_metrics.started_at, EXCLUDED.started_at))) > 0
				THEN (((upload_metrics.bytes + EXCLUDED.bytes)::DOUBLE PRECISION * 8 / EXTRACT(EPOCH FROM (GREATEST(upload_metrics.finished_at, EXCLUDED.finished_at) - LEAST(upload_metrics.started_at, EXCLUDED.started_at))) / 1000000) / EXCLUDED.bandwidth_baseline_mbps * 100)
				ELSE 0
			END,
			bandwidth_baseline_mbps = EXCLUDED.bandwidth_baseline_mbps,
			updated_at = NOW()
	`, uploadID, bytes, startedAt, finishedAt, durationMs, averageMbps, uploadBandwidthUtilization(averageMbps), bandwidthBaselineMbps)
	if err != nil {
		log.Printf("Failed to persist upload metric summary for %s: %v", uploadID, err)
	}
}

func persistUploadMetricSummary(ctx context.Context, uploadID string, summary UploadMetricSummary) error {
	if db == nil || uploadID == "" || summary.Bytes <= 0 || summary.Duration <= 0 {
		return nil
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO upload_metrics(upload_id, bytes, started_at, finished_at, duration_ms, average_mbps, bandwidth_utilization, bandwidth_baseline_mbps, updated_at)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (upload_id) DO UPDATE SET
			bytes = EXCLUDED.bytes,
			started_at = EXCLUDED.started_at,
			finished_at = EXCLUDED.finished_at,
			duration_ms = EXCLUDED.duration_ms,
			average_mbps = EXCLUDED.average_mbps,
			bandwidth_utilization = EXCLUDED.bandwidth_utilization,
			bandwidth_baseline_mbps = EXCLUDED.bandwidth_baseline_mbps,
			updated_at = NOW()
	`, uploadID, summary.Bytes, summary.StartedAt, summary.FinishedAt, summary.Duration.Milliseconds(), summary.AverageMbps, summary.BandwidthUtilization, summary.BandwidthBaselineMbps)
	return err
}

func persistFinalUploadMetric(finalUploadID string, partialUploadIDs []string, fallbackMetric UploadMetric) {
	if db == nil || finalUploadID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if metric, exists := loadUploadMetricFromIntervals(ctx, partialUploadIDs); exists {
		if summary, ok := summarizeUploadMetric(metric); ok {
			if err := persistUploadMetricSummary(ctx, finalUploadID, summary); err != nil {
				log.Printf("Failed to persist final upload metric for %s: %v", finalUploadID, err)
			}
			return
		}
	}

	if summary, ok := summarizeUploadMetric(fallbackMetric); ok {
		if err := persistUploadMetricSummary(ctx, finalUploadID, summary); err != nil {
			log.Printf("Failed to persist final upload metric for %s: %v", finalUploadID, err)
		}
	}
}

func loadUploadMetricFromIntervals(ctx context.Context, uploadIDs []string) (UploadMetric, bool) {
	if db == nil || len(uploadIDs) == 0 {
		return UploadMetric{}, false
	}

	seen := make(map[string]bool)
	ids := make([]string, 0, len(uploadIDs))
	for _, uploadID := range uploadIDs {
		uploadID = normalizeUploadID(uploadID)
		if uploadID == "" || seen[uploadID] {
			continue
		}
		seen[uploadID] = true
		ids = append(ids, uploadID)
	}
	if len(ids) == 0 {
		return UploadMetric{}, false
	}

	placeholders := make([]string, 0, len(ids))
	args := make([]interface{}, 0, len(ids))
	for i, uploadID := range ids {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, uploadID)
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT bytes, started_at, finished_at
		FROM upload_metric_intervals
		WHERE upload_id IN (%s)
		ORDER BY started_at
	`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		log.Printf("Failed to load upload metric intervals: %v", err)
		return UploadMetric{}, false
	}
	defer rows.Close()

	var metric UploadMetric
	for rows.Next() {
		var bytes int64
		var startedAt time.Time
		var finishedAt time.Time
		if err := rows.Scan(&bytes, &startedAt, &finishedAt); err != nil {
			log.Printf("Failed to scan upload metric interval: %v", err)
			continue
		}
		if bytes <= 0 || !finishedAt.After(startedAt) {
			continue
		}
		metric.Bytes += bytes
		metric.Intervals = append(metric.Intervals, UploadMetricInterval{Start: startedAt, End: finishedAt})
	}
	if err := rows.Err(); err != nil {
		log.Printf("Failed to iterate upload metric intervals: %v", err)
		return UploadMetric{}, false
	}

	return metric, metric.Bytes > 0 && len(metric.Intervals) > 0
}

func getPersistedUploadMetric(ctx context.Context, uploadID string) (UploadMetricSummary, bool) {
	if db == nil || uploadID == "" {
		return UploadMetricSummary{}, false
	}

	var summary UploadMetricSummary
	var startedAt sql.NullTime
	var finishedAt sql.NullTime
	var durationMs int64
	err := db.QueryRowContext(ctx, `
		SELECT bytes, started_at, finished_at, duration_ms, average_mbps, bandwidth_utilization, bandwidth_baseline_mbps
		FROM upload_metrics
		WHERE upload_id = $1
	`, uploadID).Scan(
		&summary.Bytes,
		&startedAt,
		&finishedAt,
		&durationMs,
		&summary.AverageMbps,
		&summary.BandwidthUtilization,
		&summary.BandwidthBaselineMbps,
	)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("Failed to load upload metric for %s: %v", uploadID, err)
		}
		return UploadMetricSummary{}, false
	}
	if summary.Bytes <= 0 || !startedAt.Valid || !finishedAt.Valid || durationMs <= 0 {
		return UploadMetricSummary{}, false
	}

	summary.StartedAt = startedAt.Time
	summary.FinishedAt = finishedAt.Time
	summary.Duration = time.Duration(durationMs) * time.Millisecond
	return summary, true
}

func shareCodeResponse(uploadID string, code string, filename string) gin.H {
	response := gin.H{"code": code, "filename": filename}
	if metric, exists := getUploadMetric(uploadID); exists {
		if summary, ok := summarizeUploadMetric(metric); ok {
			response["upload_metric"] = uploadMetricResponse(summary)
			return response
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if summary, exists := getPersistedUploadMetric(ctx, uploadID); exists {
		response["upload_metric"] = uploadMetricResponse(summary)
	}

	return response
}

func uploadIDFromLocation(location string) string {
	if location == "" {
		return ""
	}

	parsed, err := url.Parse(location)
	if err != nil {
		return ""
	}

	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return ""
	}

	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func normalizeUploadID(value string) string {
	if uploadID := uploadIDFromLocation(value); uploadID != "" {
		return uploadID
	}
	return value
}

func rememberUploadOwner(uploadID string, ownerHash string) {
	if db == nil || uploadID == "" || ownerHash == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, `
		INSERT INTO upload_owners(upload_id, owner_key_hash)
		VALUES($1, $2)
		ON CONFLICT (upload_id) DO UPDATE SET owner_key_hash = EXCLUDED.owner_key_hash
	`, uploadID, ownerHash)
	if err != nil {
		log.Printf("Failed to remember upload owner for %s: %v", uploadID, err)
	}
}

func ownerHashForUpload(ctx context.Context, uploadID string) string {
	if db == nil || uploadID == "" {
		return ""
	}

	var ownerHash string
	err := db.QueryRowContext(ctx, "SELECT owner_key_hash FROM upload_owners WHERE upload_id=$1", uploadID).Scan(&ownerHash)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("Failed to resolve upload owner for %s: %v", uploadID, err)
	}
	return ownerHash
}

func listObjects(c *gin.Context) {
	if s3Client == nil {
		c.JSON(500, gin.H{"error": "S3 client not initialized"})
		return
	}

	output, err := s3Client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
		Bucket: aws.String(s3Bucket),
	})
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	var objects []string
	for _, obj := range output.Contents {
		objects = append(objects, *obj.Key)
	}

	c.JSON(200, gin.H{
		"bucket":  s3Bucket,
		"objects": objects,
	})
}

func headObject(c *gin.Context) {
    if s3Client == nil {
        c.JSON(500, gin.H{"error": "S3 client not initialized"})
        return
    }
    id := c.Param("id")
    _, err := s3Client.HeadObject(context.TODO(), &s3.HeadObjectInput{Bucket: aws.String(s3Bucket), Key: aws.String(id)})
    if err != nil {
        c.JSON(404, gin.H{"exists": false, "error": err.Error()})
        return
    }
    c.JSON(200, gin.H{"exists": true})
}
