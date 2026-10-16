package registry

import (
	"fmt"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// WriteImage stores an image in a repository under tag, as a push of it
// would, and returns its manifest's digest. Its layers are streamed into
// the store compressed, so an image loaded from `docker save` is stored as
// any registry client would push it (#1383).
func (s *Store) WriteImage(r Repo, image, tag string, img v1.Image) (string, error) {
	layers, err := img.Layers()
	if err != nil {
		return "", err
	}
	for _, l := range layers {
		d, err := l.Digest()
		if err != nil {
			return "", err
		}
		if _, err := s.BlobSize(d.String()); err == nil {
			continue
		}
		rc, err := l.Compressed()
		if err != nil {
			return "", err
		}
		err = s.putStream(d.String(), rc)
		rc.Close()
		if err != nil {
			return "", fmt.Errorf("store layer %s: %w", d, err)
		}
	}
	cfg, err := img.RawConfigFile()
	if err != nil {
		return "", err
	}
	if _, err := s.PutBlob(cfg); err != nil {
		return "", err
	}
	m, err := img.RawManifest()
	if err != nil {
		return "", err
	}
	mt, err := img.MediaType()
	if err != nil {
		return "", err
	}
	return s.PutManifest(r, image, tag, string(mt), m)
}

func (s *Store) putStream(digest string, rc io.Reader) error {
	id, err := s.StartUpload()
	if err != nil {
		return err
	}
	if _, err := s.AppendUpload(id, rc); err != nil {
		_ = s.CancelUpload(id)
		return err
	}
	if err := s.FinishUpload(id, digest); err != nil {
		_ = s.CancelUpload(id)
		return err
	}
	return nil
}
