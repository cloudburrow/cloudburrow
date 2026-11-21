package console

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Media playback (#2084): an audio object of a provider's ObjectStore,
// served to an <audio> element on the console's own origin, as Google's
// Speech-to-Text Transcription details page plays a transcription's audio
// in the browser.
//
// Like a preview, the response is never what the object claims to be
// unless that is an audio type on mediaTypes: anything else is refused, so
// an object labelled text/html cannot be served through this route as a
// document. Every response carries the object CSP and nosniff.
//
// The object is read into memory, up to MediaLimit, so the response can
// answer Range requests: a player seeks with them, and Chrome will not seek
// in a response that does not accept ranges.

// MediaLimit is the largest object played. Speech-to-Text's BatchRecognize
// takes up to 480 minutes of audio; a 30-minute 16 kHz mono WAV is 58 MB.
const MediaLimit int64 = 128 << 20

// mediaTypes are the audio types played, Speech-to-Text's documented
// encodings (https://docs.cloud.google.com/speech-to-text/docs/encoding)
// under the names browsers and uploaders give them.
var mediaTypes = map[string]string{
	"audio/wav": "audio/wav", "audio/x-wav": "audio/wav", "audio/wave": "audio/wav", "audio/vnd.wave": "audio/wav",
	"audio/mpeg": "audio/mpeg", "audio/mp3": "audio/mpeg",
	"audio/flac": "audio/flac", "audio/x-flac": "audio/flac",
	"audio/ogg": "audio/ogg", "audio/opus": "audio/ogg", "audio/webm": "audio/webm",
	"audio/mp4": "audio/mp4", "audio/aac": "audio/aac", "audio/basic": "audio/basic",
}

// mediaExtensions are the audio types by extension, for an object stored
// as application/octet-stream or with no type.
var mediaExtensions = map[string]string{
	".wav": "audio/wav", ".mp3": "audio/mpeg", ".flac": "audio/flac", ".ogg": "audio/ogg", ".opus": "audio/ogg",
	".webm": "audio/webm", ".m4a": "audio/mp4", ".aac": "audio/aac", ".au": "audio/basic",
}

// MediaType is the audio type an object of this name and content type is
// played as, or "" when it is not audio the console plays.
func MediaType(name, contentType string) string {
	mt, _, _ := mime.ParseMediaType(contentType)
	if t := mediaTypes[strings.ToLower(mt)]; t != "" {
		return t
	}
	if mt == "" || mt == "application/octet-stream" || mt == "binary/octet-stream" {
		return mediaExtensions[strings.ToLower(path.Ext(name))]
	}
	return ""
}

// MediaURL is the console route that plays the object path of service's
// ObjectStore: the bucket, then the object name's segments.
func MediaURL(service, project string, objectPath []string) string {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	for _, seg := range objectPath {
		q.Add("name", seg)
	}
	return "/api/objects/" + url.PathEscape(service) + "/media?" + q.Encode()
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	_, store, ok := s.objectStore(w, r)
	if !ok {
		return
	}
	obj, err := store.OpenObject(r.Context(), r.URL.Query().Get("project"), objectPath(r))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": userMessage(err)})
		return
	}
	defer obj.Close()
	objectHeaders(w)
	mt := MediaType(obj.Name, obj.ContentType)
	if mt == "" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
			"error": fmt.Sprintf("%s is %s, not audio the console plays; download it instead", path.Base(obj.Name), orUnknown(obj.ContentType)),
		})
		return
	}
	if obj.Size > MediaLimit {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("%s is %s; the console plays at most %s — download it instead",
				path.Base(obj.Name), formatSize(obj.Size), formatSize(MediaLimit)),
		})
		return
	}
	body, err := io.ReadAll(io.LimitReader(obj, MediaLimit+1))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": userMessage(err)})
		return
	}
	if int64(len(body)) > MediaLimit {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("%s is larger than the %s the console plays", path.Base(obj.Name), formatSize(MediaLimit)),
		})
		return
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}
