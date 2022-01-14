package main

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
)

var (
	rePathUpload = regexp.MustCompile(`^/upload$`)
	rePathFiles  = regexp.MustCompile(`^/files(/.*)?(/[^/]+)$`)

	errTokenMismatch = errors.New("token mismatched")
	errMissingToken  = errors.New("missing token")
)

// Server represents a simple-upload server.
type Server struct {
	DocumentRoot string
	// MaxUploadSize limits the size of the uploaded content, specified with "byte".
	MaxUploadSize    int64
	SecureToken      string
	EnableCORS       bool
	ProtectedMethods []string
}

// NewServer creates a new simple-upload server.
func NewServer(documentRoot string, maxUploadSize int64, token string, enableCORS bool, protectedMethods []string) Server {
	return Server{
		DocumentRoot:     documentRoot,
		MaxUploadSize:    maxUploadSize,
		SecureToken:      token,
		EnableCORS:       enableCORS,
		ProtectedMethods: protectedMethods,
	}
}

func (s Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if !rePathFiles.MatchString(r.URL.Path) {
		w.WriteHeader(http.StatusNotFound)
		writeError(w, fmt.Errorf("\"%s\" is not found", r.URL.Path))
		return
	}
	if s.EnableCORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	http.StripPrefix("/files/", http.FileServer(http.Dir(s.DocumentRoot))).ServeHTTP(w, r)
}

func (s Server) handlePost(w http.ResponseWriter, r *http.Request) {
	srcFile, info, err := r.FormFile("file")
	if err != nil {
		logger.WithError(err).Error("failed to acquire the uploaded content")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}
	defer srcFile.Close()
	logger.Debug(info)
	size, err := getSize(srcFile)
	if err != nil {
		logger.WithError(err).Error("failed to get the size of the uploaded content")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}
	if size > s.MaxUploadSize {
		logger.WithField("size", size).Info("file size exceeded")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		writeError(w, errors.New("uploaded file size exceeds the limit"))
		return
	}

	body, err := ioutil.ReadAll(srcFile)
	if err != nil {
		logger.WithError(err).Error("failed to read the uploaded content")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}
	filename := info.Filename
	if filename == "" {
		filename = fmt.Sprintf("%x", sha1.Sum(body))
	}

	dstPath := path.Join(s.DocumentRoot, filename)
	dstFile, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		logger.WithError(err).WithField("path", dstPath).Error("failed to open the file")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}
	defer dstFile.Close()
	if written, err := dstFile.Write(body); err != nil {
		logger.WithError(err).WithField("path", dstPath).Error("failed to write the content")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	} else if int64(written) != size {
		logger.WithFields(logrus.Fields{
			"size":    size,
			"written": written,
		}).Error("uploaded file size and written size differ")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, fmt.Errorf("the size of uploaded content is %d, but %d bytes written", size, written))
	}
	uploadedURL := strings.TrimPrefix(dstPath, s.DocumentRoot)
	if !strings.HasPrefix(uploadedURL, "/") {
		uploadedURL = "/" + uploadedURL
	}
	uploadedURL = "/files" + uploadedURL
	logger.WithFields(logrus.Fields{
		"path": dstPath,
		"url":  uploadedURL,
		"size": size,
	}).Info("file uploaded by POST")
	if s.EnableCORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.WriteHeader(http.StatusOK)
	writeSuccess(w, uploadedURL)
}

func (s Server) handlePut(w http.ResponseWriter, r *http.Request) {
	matches := rePathFiles.FindStringSubmatch(r.URL.Path)
	if matches == nil {
		logger.WithField("path", r.URL.Path).Info("invalid path")
		w.WriteHeader(http.StatusNotFound)
		writeError(w, fmt.Errorf("\"%s\" is not found", r.URL.Path))
		return
	}
	targetDir := path.Join(s.DocumentRoot, matches[1])
	targetFilename := matches[2]
	targetPath := path.Join(targetDir, targetFilename)

	// We have to create a new temporary file in the same device to avoid "invalid cross-device link" on renaming.
	// Here is the easiest solution: create it in the same directory.
	tempFile, err := ioutil.TempFile(s.DocumentRoot, "upload_")
	if err != nil {
		logger.WithError(err).Error("failed to create a temporary file")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}
	defer r.Body.Close()
	srcFile, info, err := r.FormFile("file")
	if err != nil {
		logger.WithError(err).WithField("path", targetPath).Error("failed to acquire the uploaded content")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}
	defer srcFile.Close()
	// dump headers for the file
	logger.Debug(info.Header)

	size, err := getSize(srcFile)
	if err != nil {
		logger.WithError(err).WithField("path", targetPath).Error("failed to get the size of the uploaded content")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}
	if size > s.MaxUploadSize {
		logger.WithFields(logrus.Fields{
			"path": targetPath,
			"size": size,
		}).Info("file size exceeded")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		writeError(w, errors.New("uploaded file size exceeds the limit"))
		return
	}

	n, err := io.Copy(tempFile, srcFile)
	if err != nil {
		logger.WithError(err).WithField("path", tempFile.Name()).Error("failed to write body to the file")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}
	// excplicitly close file to flush, then rename from temp name to actual name in atomic file
	// operation if on linux or other unix-like OS (windows hosts should look into https://github.com/natefinch/atomic
	// package for atomic file write operations)
	tempFile.Close()
	
	if err := os.MkdirAll(targetDir, 0777); err != nil {
		os.Remove(tempFile.Name())
		logger.WithError(err).WithField("path", targetPath).Error("failed to create directories")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
        }
	
	if err := os.Rename(tempFile.Name(), targetPath); err != nil {
		os.Remove(tempFile.Name())
		logger.WithError(err).WithField("path", targetPath).Error("failed to rename temp file to final filename for upload")
		w.WriteHeader(http.StatusInternalServerError)
		writeError(w, err)
		return
	}

	logger.WithFields(logrus.Fields{
		"path": r.URL.Path,
		"size": n,
	}).Info("file uploaded by PUT")
	if s.EnableCORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.WriteHeader(http.StatusOK)
	writeSuccess(w, r.URL.Path)
}

func (s Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	var allowedMethods []string
	if rePathFiles.MatchString(r.URL.Path) {
		allowedMethods = []string{http.MethodPut, http.MethodGet, http.MethodHead}
	} else if rePathUpload.MatchString(r.URL.Path) {
		allowedMethods = []string{http.MethodPost}
	} else {
		w.WriteHeader(http.StatusNotFound)
		writeError(w, errors.New("not found"))
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(allowedMethods, ","))
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) checkToken(r *http.Request) error {
	// first, try to get the token from the query strings
	token := r.URL.Query().Get("token")
	// if token is not found, check the form parameter.
	if token == "" {
		token = r.FormValue("token")
	}
	if token == "" {
		return errMissingToken
	}
	if token != s.SecureToken {
		return errTokenMismatch
	}
	return nil
}

func (s Server) isAuthenticationRequired(r *http.Request) bool {
	for _, m := range s.ProtectedMethods {
		if m == r.Method {
			return true
		}
	}
	return false
}

func (s Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.checkToken(r); s.isAuthenticationRequired(r) && err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		writeError(w, err)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleGet(w, r)
	case http.MethodPost:
		s.handlePost(w, r)
	case http.MethodPut:
		s.handlePut(w, r)
	case http.MethodOptions:
		s.handleOptions(w, r)
	default:
		w.Header().Add("Allow", "GET,HEAD,POST,PUT")
		w.WriteHeader(http.StatusMethodNotAllowed)
		writeError(w, fmt.Errorf("method \"%s\" is not allowed", r.Method))
	}
}
