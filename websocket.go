package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"golang.org/x/net/websocket"
)

var (
	activeWSMu sync.Mutex
	activeWS   = make(map[string]context.CancelFunc)
	isTesting  = false
)

type wsPinMessage struct {
	Event       string `json:"event"`
	MediaType   string `json:"media_type"`
	IMDbID      string `json:"imdb_id"`
	TMDbID      string `json:"tmdb_id"`
	TVDbID      string `json:"tvdb_id"`
	VirtualPath string `json:"virtual_path"`
}

func ensureWSClient(wispURL, token string) {
	if isTesting {
		return
	}
	activeWSMu.Lock()
	defer activeWSMu.Unlock()

	if _, ok := activeWS[wispURL]; ok {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	activeWS[wispURL] = cancel

	go runWSClient(ctx, wispURL, token)
}

func runWSClient(ctx context.Context, wispURL, token string) {
	wsURL := wispURL
	if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	} else if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	}
	wsURL = strings.TrimSuffix(wsURL, "/") + "/api/ws"

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := func() error {
			config, err := websocket.NewConfig(wsURL, "http://localhost/")
			if err != nil {
				return err
			}
			if token != "" {
				config.Header.Set("Authorization", "Bearer "+token)
			}
			conn, err := websocket.DialConfig(config)
			if err != nil {
				return err
			}
			defer conn.Close()

			var raw string
			for {
				if err := websocket.Message.Receive(conn, &raw); err != nil {
					return err
				}
				var msg wsPinMessage
				if err := json.Unmarshal([]byte(raw), &msg); err != nil {
					continue
				}
				if msg.Event == "pin_completed" {
					triggerDBScan(context.Background(), msg.MediaType, msg.VirtualPath)
				}
			}
		}()

		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func triggerDBScan(ctx context.Context, mediaType, vpath string) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return
	}
	defer db.Close()

	parts := strings.Split(vpath, "/")
	if len(parts) < 2 {
		return
	}
	subpath := strings.Join(parts[1:], "/")

	dbType := "movies"
	if mediaType == "series" {
		dbType = "series"
	}

	rows, err := db.QueryContext(ctx, `
		SELECT f.id, fp.path FROM media_folders f
		JOIN media_folder_paths fp ON fp.media_folder_id = f.id
		WHERE f.type = $1 AND f.enabled = true`, dbType)
	if err != nil {
		return
	}
	defer rows.Close()

	var folderID int
	var matchedPath string
	for rows.Next() {
		var fid int
		var p string
		if err := rows.Scan(&fid, &p); err != nil {
			continue
		}
		folderID = fid
		matchedPath = p
		break
	}

	if matchedPath == "" {
		return
	}

	dirPath := filepath.Dir(filepath.Join(matchedPath, subpath))

	scanID := randULID()
	_, err = db.ExecContext(ctx, `
		INSERT INTO scan_runs (id, media_folder_id, mode, path, trigger, status, result_payload, error_message)
		VALUES ($1, $2, 'subtree', $3, 'plugin', 'pending', '{}', '')`,
		scanID, folderID, dirPath)
	if err != nil {
		log.Printf("triggerDBScan error: %v", err)
	}
}

func randULID() string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUV"
	b := make([]byte, 26)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}
