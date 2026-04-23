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
    "strings"
    "sync"
    "time"
    "strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/gin-contrib/cors"
    "github.com/gin-gonic/gin"
    "github.com/tus/tusd/v2/pkg/filestore"
    "github.com/tus/tusd/v2/pkg/handler"
    "github.com/tus/tusd/v2/pkg/s3store"
    _ "github.com/lib/pq"
)

// Global Storage
var (
    codeToUpload = make(map[string]UploadRecord)
    mu           sync.RWMutex
    
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
	codeRegex       = regexp.MustCompile(`^[A-Za-z0-9._-]{4,128}$`)
	apiKey          string // Legacy API key for authentication (from env)
	adminKey        string // Admin key for managing API keys (optional)
)

type UploadRecord struct {
	UploadID string `json:"upload_id"`
	Filename string `json:"filename"`
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
    if n, err := strconv.Atoi(ttlMinutesStr); err == nil && n > 0 { ttlMinutes = n }
    maxDownloads = 10000
    if n, err := strconv.Atoi(maxDownloadsStr); err == nil && n > 0 { maxDownloads = n }
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

	// Create tusd handler
	tusConfig := handler.Config{
		BasePath:              "/files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
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
            
            filename := extractFilename(event.Upload.MetaData)

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
				
				// Retry loop for short code generation
				for i := 0; i < 5; i++ {
					shortCode = genCode(8) // Generate 8-char code
					exp := time.Now().UTC().Add(time.Duration(ttlMinutes) * time.Minute)
					
					// Try insert
					res, err := db.ExecContext(ctx, 
						"INSERT INTO share_codes(code, upload_id, filename, expires_at, max_downloads, downloads) VALUES($1,$2,$3,$4,$5,0) ON CONFLICT (code) DO NOTHING", 
						shortCode, uploadID, filename, exp, maxDownloads)
					
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
					_, _ = db.ExecContext(ctx, "INSERT INTO share_codes(code, upload_id, filename, max_downloads) VALUES($1,$2,$3,$4) ON CONFLICT (code) DO UPDATE SET filename=EXCLUDED.filename", uploadID, uploadID, filename, maxDownloads)
				}
			}

            log.Printf("Event: Upload completed - ID: %s, Filename: %s", uploadID, filename)
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
        AllowHeaders:  []string{"Origin", "Content-Type", "Upload-Length", "Upload-Metadata", "Tus-Resumable", "Upload-Offset", "Authorization", "X-API-Key", "X-Admin-Key"},
        ExposeHeaders: []string{"Upload-Length", "Upload-Metadata", "Tus-Resumable", "Upload-Offset", "Location", "Tus-Version"},
        AllowCredentials: false,
    }))

	// Files Handler (Custom GET + Tusd)
	r.Any("/files/*any", apiKeyAuth(), func(c *gin.Context) {
		path := c.Param("any")
		uploadID := strings.TrimPrefix(path, "/")

        // 1. Handle Download (GET)
        if c.Request.Method == "GET" && uploadID != "" {
            filename := "download"
			
			mu.RLock()
			record, exists := codeToUpload[uploadID]
			mu.RUnlock()
			
			if exists {
				filename = record.Filename
			} else {
				if upload, err := dataStore.GetUpload(context.Background(), uploadID); err == nil {
					if info, err := upload.GetInfo(context.Background()); err == nil {
						filename = extractFilename(info.MetaData)
						mu.Lock()
						codeToUpload[uploadID] = UploadRecord{UploadID: uploadID, Filename: filename}
						mu.Unlock()
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
                    fname = extractFilename(info.MetaData)
                    exp := time.Now().UTC().Add(time.Duration(ttlMinutes) * time.Minute)
                    
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

			// S3 Download (Redirect to Presigned URL)
			if s3Client != nil {
				targetKey := uploadID
				
				// 1. Check if object exists (Fast Path)
				_, err := s3Client.HeadObject(context.TODO(), &s3.HeadObjectInput{
					Bucket: aws.String(s3Bucket),
					Key:    aws.String(targetKey),
				})
				
				// 2. If not found, try to find it via Prefix Search (Robust Path)
				if err != nil {
					log.Printf("Key %s not found directly, trying prefix search...", targetKey)
					
					// Robust Prefix Strategy:
					// Tusd IDs often start with a 32-char UUID. Use the first 32 chars as prefix.
					// If ID is shorter than 32, use the whole ID.
					prefix := targetKey
					if len(targetKey) > 32 {
						prefix = targetKey[:32]
					}
					
					// List objects with prefix
					listOut, lerr := s3Client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
						Bucket: aws.String(s3Bucket),
						Prefix: aws.String(prefix),
						MaxKeys: 10,
					})
					
					if lerr == nil {
						for _, obj := range listOut.Contents {
							k := *obj.Key
							// Skip .info files
							if strings.HasSuffix(k, ".info") {
								continue
							}
							// Found a candidate (the data file)
							targetKey = k
							log.Printf("Resolved S3 Key: %s -> %s", uploadID, targetKey)
							break
						}
					}
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
		http.StripPrefix("/files/", tusHandler).ServeHTTP(c.Writer, c.Request)
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
        api.POST("/get-code", getShareCode)
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

func getShareCode(c *gin.Context) {
    var req struct {
        UploadID string `json:"upload_id" binding:"required"`
    }
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(400, gin.H{"error": "upload_id is required"})
        return
    }

    if db != nil {
        var code string
        var filename string
        var expires sql.NullTime
        var downloads int
        var maxd int
        
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        err := db.QueryRowContext(ctx, "SELECT code, filename, expires_at, downloads, max_downloads FROM share_codes WHERE upload_id=$1 ORDER BY created_at DESC LIMIT 1", req.UploadID).Scan(&code, &filename, &expires, &downloads, &maxd)
        if err == nil {
            // Upgrade legacy long code (where code == uploadID) to short code
            // Only if code equals uploadID (meaning it was auto-inserted as fallback or legacy)
            if code != req.UploadID {
                 if (!expires.Valid || time.Now().UTC().Before(expires.Time)) && (maxd == 0 || downloads < maxd) {
                    c.JSON(200, gin.H{"code": code, "filename": filename})
                    return
                 }
            }
        }
        
        // Conflict Retry Logic
        var newCode string
        for i := 0; i < 5; i++ {
             newCode = genCode(8)
             exp := time.Now().UTC().Add(time.Duration(ttlMinutes) * time.Minute)
             res, ierr := db.ExecContext(ctx, "INSERT INTO share_codes(code, upload_id, filename, expires_at, max_downloads, downloads) VALUES($1,$2,$3,$4,$5,0) ON CONFLICT (code) DO NOTHING", newCode, req.UploadID, filenameOrCache(req.UploadID), exp, maxDownloads)
             if ierr == nil {
                 if rows, _ := res.RowsAffected(); rows > 0 {
                     c.JSON(200, gin.H{"code": newCode, "filename": filenameOrCache(req.UploadID)})
                     return
                 }
             }
        }
        
        // Fallback to UploadID as code
        c.JSON(200, gin.H{"code": req.UploadID, "filename": filenameOrCache(req.UploadID)})
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
        
        exp := time.Now().UTC().Add(time.Duration(ttlMinutes) * time.Minute)
        _, _ = db.ExecContext(ctx, "INSERT INTO share_codes(code, upload_id, filename, expires_at, max_downloads) VALUES($1,$2,$3,$4,$5) ON CONFLICT (code) DO UPDATE SET filename=EXCLUDED.filename", req.UploadID, req.UploadID, record.Filename, exp, maxDownloads)
    }

    c.JSON(200, gin.H{"code": req.UploadID, "filename": record.Filename})
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
        resolved := extractFilename(info.MetaData)
        exp := time.Now().UTC().Add(time.Duration(ttlMinutes) * time.Minute)
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
