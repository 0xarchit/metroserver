package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"go.etcd.io/bbolt"
	"go.uber.org/zap"
)

const (
	DefaultDatabaseFile  = "metroserver.db"
	MaxUserAgentLength   = 512
	MaxTrackedUserAgents = 10000
	databaseLockTimeout  = time.Second
)

var userAgentsBucket = []byte("user_agents")

type database struct {
	db *bbolt.DB
}

func openDatabase(path string) (*database, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: databaseLockTimeout})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &database{db: db}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(userAgentsBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (d *database) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

func (d *database) recordUserAgent(userAgent string) error {
	if d == nil || d.db == nil {
		return nil
	}
	userAgent = sanitizeString(userAgent, MaxUserAgentLength)
	if userAgent == "" {
		return nil
	}

	return d.db.Batch(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(userAgentsBucket)
		key := []byte(userAgent)
		value := bucket.Get(key)
		if value == nil {
			if bucket.Sequence() >= MaxTrackedUserAgents {
				return nil
			}
			if _, err := bucket.NextSequence(); err != nil {
				return err
			}
		} else if len(value) != 8 {
			return errors.New("invalid User-Agent count in database")
		}

		count := uint64(1)
		if value != nil {
			count = binary.BigEndian.Uint64(value)
			if count < ^uint64(0) {
				count++
			}
		}
		encoded := make([]byte, 8)
		binary.BigEndian.PutUint64(encoded, count)
		return bucket.Put(key, encoded)
	})
}

func (d *database) userAgentCounts() (map[string]uint64, error) {
	counts := make(map[string]uint64)
	if d == nil || d.db == nil {
		return counts, nil
	}
	err := d.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(userAgentsBucket).ForEach(func(key, value []byte) error {
			if len(value) != 8 {
				return errors.New("invalid User-Agent count in database")
			}
			counts[string(key)] = binary.BigEndian.Uint64(value)
			return nil
		})
	})
	return counts, err
}

func (s *Server) userAgentsHandler(adminToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		provided, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		expectedHash := sha256.Sum256([]byte(adminToken))
		providedHash := sha256.Sum256([]byte(provided))
		if adminToken == "" || subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		counts, err := s.database.userAgentCounts()
		if err != nil {
			s.logger.Error("Failed to read User-Agent database", zap.Error(err))
			http.Error(w, "failed to read User-Agent database", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(counts)
	}
}
