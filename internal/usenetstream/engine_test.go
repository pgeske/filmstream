package usenetstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreateAndServeRange(t *testing.T) {
	const media = "0123456789"
	var mu sync.Mutex
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/release.nzb":
			w.Header().Set("Content-Type", "application/x-nzb")
			fmt.Fprint(w, `<?xml version="1.0"?><nzb/>`)
		case r.URL.Path == "/api" && r.Method == http.MethodPost:
			if r.Header.Get("X-Api-Key") != "api-secret" || r.URL.Query().Get("apikey") != "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("mode") != "addfile" || r.FormValue("cat") != "movies" || r.FormValue("priority") != "2" {
				t.Errorf("unexpected add form: %+v", r.MultipartForm.Value)
			}
			file, _, err := r.FormFile("nzbFile")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			contents, _ := io.ReadAll(file)
			if !strings.Contains(string(contents), "<nzb") {
				t.Errorf("NZB = %q", contents)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":true,"nzo_ids":["job-1"]}`)
		case r.URL.Path == "/api" && r.URL.Query().Get("mode") == "history" && r.URL.Query().Get("name") == "delete":
			mu.Lock()
			deleted = true
			mu.Unlock()
			fmt.Fprint(w, `{"status":true}`)
		case r.URL.Path == "/api" && r.URL.Query().Get("mode") == "queue" && r.URL.Query().Get("name") == "delete":
			fmt.Fprint(w, `{"status":true}`)
		case r.URL.Path == "/api" && r.URL.Query().Get("mode") == "history":
			if r.URL.Query().Get("nzo_ids") != "job-1" {
				t.Errorf("nzo_ids = %q", r.URL.Query().Get("nzo_ids"))
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"history":{"slots":[{"nzo_id":"job-1","status":"Completed","category":"movies","storage":"/data/completed/movies/Test Release"}]}}`)
		case r.Method == "PROPFIND" && r.URL.Path == "/content/movies/Test Release":
			user, password, ok := r.BasicAuth()
			if !ok || user != "dav-user" || password != "dav-secret" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">
				<d:response><d:href>/content/movies/Test%20Release</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
				<d:response><d:href>/content/movies/Test%20Release/sample.mkv</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>3</d:getcontentlength><d:getcontenttype>video/x-matroska</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
				<d:response><d:href>/content/movies/Test%20Release/movie.mkv</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>10</d:getcontentlength><d:getcontenttype>video/x-matroska</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
			</d:multistatus>`)
		case r.URL.Path == "/content/movies/Test Release/movie.mkv":
			user, password, ok := r.BasicAuth()
			if !ok || user != "dav-user" || password != "dav-secret" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Type", "video/x-matroska")
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "10")
				return
			}
			if r.Header.Get("Range") == "bytes=2-5" {
				w.Header().Set("Content-Length", "4")
				w.Header().Set("Content-Range", "bytes 2-5/10")
				w.WriteHeader(http.StatusPartialContent)
				fmt.Fprint(w, media[2:6])
				return
			}
			fmt.Fprint(w, media)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	engine, err := New(Config{
		BaseURL: server.URL, APIKey: "api-secret",
		WebDAVUser: "dav-user", WebDAVPassword: "dav-secret",
		Category: "movies", StartupTimeout: 5 * time.Second,
		IdleGrace: time.Hour, CleanupInterval: time.Hour,
		ControlClient: server.Client(), StreamClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	session, err := engine.Create(context.Background(), Source{NZBURL: server.URL + "/release.nzb", Name: "Test Release"})
	if err != nil {
		t.Fatal(err)
	}
	if session.FileName != "movie.mkv" || session.FileSize != int64(len(media)) {
		t.Fatalf("session = %+v", session)
	}

	request := httptest.NewRequest(http.MethodGet, "/stream", nil)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	if err := engine.ServeHTTP(response, request, session.ID); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusPartialContent || response.Body.String() != "2345" {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	status, ok := engine.Status(session.ID)
	if !ok || status.Source != "usenet" || status.DownloadedBytes != 4 {
		t.Fatalf("status = %+v, ok = %v", status, ok)
	}
	if err := engine.Drop(session.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	wasDeleted := deleted
	mu.Unlock()
	if !wasDeleted {
		t.Fatal("history item was not deleted")
	}
}

func TestCreateReportsFailedPreparation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/release.nzb":
			fmt.Fprint(w, `<nzb/>`)
		case r.Method == http.MethodPost:
			fmt.Fprint(w, `{"status":true,"nzo_ids":["job-1"]}`)
		case r.URL.Query().Get("mode") == "history" && r.URL.Query().Get("name") == "":
			fmt.Fprint(w, `{"history":{"slots":[{"nzo_id":"job-1","status":"Failed","fail_message":"articles are unavailable"}]}}`)
		default:
			fmt.Fprint(w, `{"status":true}`)
		}
	}))
	defer server.Close()
	engine, err := New(Config{
		BaseURL: server.URL, APIKey: "key", WebDAVUser: "user", WebDAVPassword: "pass",
		StartupTimeout: 2 * time.Second, IdleGrace: time.Hour, CleanupInterval: time.Hour,
		ControlClient: server.Client(), StreamClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	_, err = engine.Create(context.Background(), Source{NZBURL: server.URL + "/release.nzb"})
	if err == nil || !strings.Contains(err.Error(), "articles are unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestSanitizeNZBName(t *testing.T) {
	if got := sanitizeNZBName(" Movie/Name "); got != "Movie_Name.nzb" {
		t.Fatalf("name = %q", got)
	}
}
