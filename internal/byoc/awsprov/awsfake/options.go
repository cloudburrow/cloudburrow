package awsfake

import (
	"encoding/base64"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov"
)

// Options are awsprov.Options that send every service to the fake, with a
// short poll so waits on a VM's state end at once.
func (s *Server) Options() awsprov.Options {
	return awsprov.Options{Endpoint: s.URL(), PollInterval: time.Millisecond}
}

func decodeB64(v string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(v)
	return string(b), err
}
