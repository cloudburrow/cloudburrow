package localai

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Google's on-device landmark classifiers, which Vision's
// LANDMARK_DETECTION runs locally (internal/imagelandmark, #1799). Google
// publishes them on Kaggle Models as google/landmarks
// (https://www.kaggle.com/models/google/landmarks, read 2026-11-18;
// formerly tfhub.dev's on_device_vision/classifier/landmarks_classifier_*),
// one TFLite model a continent, Apache-2.0 (the model page's licence and
// each file's own TFLite metadata), trained on the Google Landmarks Dataset
// v2. They are file downloads, not API calls: each is fetched once, the
// first time an image's landmarks are asked for, from Kaggle's public
// download of the model variation's version 1 (a gzip tar holding
// 1.tflite), into <state-dir>/models; the archive and the model in it are
// both checked against the SHA-256 pinned here, measured on the files
// downloaded on 2026-11-18. They are not catalogue entries.

// LandmarkClassifier is one continent's classifier: its pinned archive
// and the model file inside it.
type LandmarkClassifier struct {
	// Region is the Kaggle variation's continent, as in
	// "classifier-<Region>-v1".
	Region string
	Model  Model
	// TFLite is the model file the archive holds, extracted next to it.
	TFLite PinnedFile
}

// landmarkVariation is the Kaggle variation's download base.
func landmarkVariation(region string) string {
	return "https://www.kaggle.com/api/v1/models/google/landmarks/tfLite/classifier-" + region + "-v1/1"
}

func landmarkClassifier(region string, archiveBytes int64, archiveSHA string, tfliteBytes int64, tfliteSHA string) LandmarkClassifier {
	return LandmarkClassifier{
		Region: region,
		Model: Model{
			ID:        "google-landmarks-classifier-" + region + "-v1",
			Repo:      "kaggle-google-landmarks/classifier-" + region + "-v1",
			Publisher: PublisherGoogle,
			Access:    AccessOpen,
			License:   "apache-2.0",
			Modality:  ModalityImageClassification,
			Artifact:  "download",
			Runtime:   "tflitemodel",
			Official:  true,
			Notes:     "Google's on-device landmark classifier for " + region + " (Apache-2.0), fetched on demand for Vision LANDMARK_DETECTION.",
			Pin: &Pin{
				Revision: "tflite-1",
				BaseURL:  landmarkVariation(region),
				Files:    []PinnedFile{{Path: "download", Bytes: archiveBytes, SHA256: archiveSHA}},
			},
		},
		TFLite: PinnedFile{Path: "landmarks_classifier_" + region + "_v1.tflite", Bytes: tfliteBytes, SHA256: tfliteSHA},
	}
}

// LandmarkClassifiers are the six continents' classifiers, together the
// world: Vision's LANDMARK_DETECTION runs every one on an image.
var LandmarkClassifiers = []LandmarkClassifier{
	landmarkClassifier("africa", 20837994, "8defc478be0de3bc3a2f3475ef6abafc8607b35cc42ac01de108676cf6eb737f",
		23524241, "c028020601f3245ba5fda4fbc295dce9e920caf52078917daa0c96930e3b4fce"),
	landmarkClassifier("asia", 41210816, "9eb9a9f8f254d0644940213062d2127961d325a347b1317743a1471f5b08f792",
		49672026, "1ae50c9a4f11890560636712fe770356a1bd3d322f8753b088d1b51f53d7cc14"),
	landmarkClassifier("europe", 41497005, "3e8efa3e9e380e7e6a0ee3bfb61bb3d75b93fcaa9107f08eb5bb34382aa6aa32",
		50056052, "4a486cab99c1296a5fd73e70bd111046a3541e798555271299999e31002a17f5"),
	landmarkClassifier("north-america", 41279396, "490ea505de6e8706912ec0c3f11af05adcfa55308ce71c56554adc91767ca4f9",
		50908148, "f0c8df5bc0de1fee3a53db904f600896b7b57eedea9afe422d9aed6f94669021"),
	landmarkClassifier("oceania-antarctica", 30307256, "9e10bf81b8a608311d88a412cf5d3fe9e57eaf356301ef8051130c955e12d76c",
		36580969, "3d181a72229bbb4d61afdea908b17d9b4ec751eaa4407dc3a70f1d1c3e469ed3"),
	landmarkClassifier("south-america", 41164133, "00567932a69aced56bf7e1a1e17d263c3c2d77e1d392c8ea07173544c75a1fc3",
		50492916, "30a21efe6914b10044d336fec4dbb2d8845102fe1b2cff880a03f0058d240649"),
}

// landmarkMember is the model file's name inside the archive.
const landmarkMember = "1.tflite"

// FetchLandmarkClassifier returns where l's model file is cached. When it
// is missing or does not match its SHA-256, the pinned archive is fetched
// (and checked), the model extracted from it and checked, and the archive
// removed.
func (c Cache) FetchLandmarkClassifier(ctx context.Context, l LandmarkClassifier) (string, error) {
	dst := filepath.Join(c.PinnedDir(l.Model), l.TFLite.Path)
	if got, err := fileSHA256(dst); err == nil && got == l.TFLite.SHA256 {
		return dst, nil
	}
	if err := PreflightDisk(c.Dir, l.Model.Pin.TotalBytes()+l.TFLite.Bytes); err != nil {
		return "", err
	}
	archive, err := c.FetchPinnedFile(ctx, l.Model, l.Model.Pin.Files[0])
	if err != nil {
		return "", err
	}
	if err := extractLandmarkModel(archive, dst, l.TFLite); err != nil {
		return "", fmt.Errorf("extract %s: %w", l.Model.ID, err)
	}
	_ = os.Remove(archive)
	return dst, nil
}

// extractLandmarkModel writes the archive's 1.tflite to dst, through a
// temporary file, once it matches want's size and SHA-256.
func extractLandmarkModel(archive, dst string, want PinnedFile) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the archive holds no %s", landmarkMember)
		}
		if err != nil {
			return err
		}
		if filepath.Clean(h.Name) != landmarkMember || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size != want.Bytes {
			return fmt.Errorf("%s is %d bytes, want %d", landmarkMember, h.Size, want.Bytes)
		}
		tmp := dst + ".part"
		out, err := os.Create(tmp)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, want.Bytes))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			var got string
			if got, err = fileSHA256(tmp); err == nil && got != want.SHA256 {
				err = fmt.Errorf("%w: %s: have %s, want %s", ErrChecksumMismatch, landmarkMember, got, want.SHA256)
			}
		}
		if err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, dst)
	}
}
