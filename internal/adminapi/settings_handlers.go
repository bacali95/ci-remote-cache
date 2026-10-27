package adminapi

import (
	"errors"
	"net/http"
	"time"

	"ci-remote-cache/internal/settings"
	"ci-remote-cache/internal/store"
)

type settingsResponse struct {
	StorageBackend string `json:"storageBackend"`
	LocalDir       string `json:"localDir"`

	S3Bucket             string `json:"s3Bucket"`
	S3Region             string `json:"s3Region"`
	S3Prefix             string `json:"s3Prefix"`
	S3Endpoint           string `json:"s3Endpoint"`
	S3UsePathStyle       bool   `json:"s3UsePathStyle"`
	S3AccessKeyIDSet     bool   `json:"s3AccessKeyIdSet"`
	S3SecretAccessKeySet bool   `json:"s3SecretAccessKeySet"`

	GCSBucket         string `json:"gcsBucket"`
	GCSPrefix         string `json:"gcsPrefix"`
	GCSCredentialsSet bool   `json:"gcsCredentialsSet"`

	SessionTTLSeconds  int   `json:"sessionTtlSeconds"`
	MaxCacheEntryBytes int64 `json:"maxCacheEntryBytes"`

	UpdatedAt time.Time `json:"updatedAt"`
}

func settingsDTO(s settings.Settings) settingsResponse {
	return settingsResponse{
		StorageBackend:       string(s.StorageBackend),
		LocalDir:             s.LocalDir,
		S3Bucket:             s.S3Bucket,
		S3Region:             s.S3Region,
		S3Prefix:             s.S3Prefix,
		S3Endpoint:           s.S3Endpoint,
		S3UsePathStyle:       s.S3UsePathStyle,
		S3AccessKeyIDSet:     s.S3AccessKeyIDSet,
		S3SecretAccessKeySet: s.S3SecretAccessKeySet,
		GCSBucket:            s.GCSBucket,
		GCSPrefix:            s.GCSPrefix,
		GCSCredentialsSet:    s.GCSCredentialsJSONSet,
		SessionTTLSeconds:    int(s.SessionTTL / time.Second),
		MaxCacheEntryBytes:   s.MaxCacheEntryBytes,
		UpdatedAt:            s.UpdatedAt,
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request, _ store.User) {
	writeJSON(w, http.StatusOK, settingsDTO(s.settings.Current()))
}

type updateSettingsRequest struct {
	StorageBackend string `json:"storageBackend"`
	LocalDir       string `json:"localDir"`

	S3Bucket          string  `json:"s3Bucket"`
	S3Region          string  `json:"s3Region"`
	S3Prefix          string  `json:"s3Prefix"`
	S3Endpoint        string  `json:"s3Endpoint"`
	S3UsePathStyle    bool    `json:"s3UsePathStyle"`
	S3AccessKeyID     *string `json:"s3AccessKeyId"`     // nil = unchanged, "" = clear
	S3SecretAccessKey *string `json:"s3SecretAccessKey"` // nil = unchanged, "" = clear

	GCSBucket          string  `json:"gcsBucket"`
	GCSPrefix          string  `json:"gcsPrefix"`
	GCSCredentialsJSON *string `json:"gcsCredentialsJson"` // nil = unchanged, "" = clear

	SessionTTLSeconds  int   `json:"sessionTtlSeconds"`
	MaxCacheEntryBytes int64 `json:"maxCacheEntryBytes"`
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request, current store.User) {
	var req updateSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	applied, err := s.settings.Apply(r.Context(), settings.ApplyInput{
		StorageBackend:     store.StorageBackendType(req.StorageBackend),
		LocalDir:           req.LocalDir,
		S3Bucket:           req.S3Bucket,
		S3Region:           req.S3Region,
		S3Prefix:           req.S3Prefix,
		S3Endpoint:         req.S3Endpoint,
		S3UsePathStyle:     req.S3UsePathStyle,
		S3AccessKeyID:      req.S3AccessKeyID,
		S3SecretAccessKey:  req.S3SecretAccessKey,
		GCSBucket:          req.GCSBucket,
		GCSPrefix:          req.GCSPrefix,
		GCSCredentialsJSON: req.GCSCredentialsJSON,
		SessionTTL:         time.Duration(req.SessionTTLSeconds) * time.Second,
		MaxCacheEntryBytes: req.MaxCacheEntryBytes,
		UpdatedBy:          current.ID,
	})
	if errors.Is(err, settings.ErrInvalidSettings) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.log.Error("apply settings failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, settingsDTO(applied))
}
