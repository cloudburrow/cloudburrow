package localai

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TessdataFastRepo is the GitHub repository of Tesseract's fast LSTM models.
const TessdataFastRepo = "tesseract-ocr/tessdata_fast"

// TessdataFastScripts are tessdata_fast 4.1.0's script models (#1733),
// Apache-2.0, which internal/ocr reads a scanned page with when orientation
// and script detection finds a script other than Latin (Latin is read with
// the eng model embedded in the binary). Each is fetched on demand, the first
// time a page in its script is read, into <state-dir>/models and checked
// against the SHA-256 pinned here. Revision is the commit the 4.1.0 tag
// names; the sizes and SHA-256s were measured on the files downloaded from
// it on 2026-11-17, each also matching the git blob ID GitHub's API reports.
// script/Latin is not pinned: the embedded eng model reads Latin pages.
// It is not a catalogue entry: no Local models screen manages it, since
// document_ocr fetches what it needs.
var TessdataFastScripts = Model{
	ID:        "tessdata-fast-4.1.0-scripts",
	Repo:      TessdataFastRepo,
	Publisher: PublisherCommunity,
	Access:    AccessOpen,
	License:   "apache-2.0",
	Modality:  ModalityOCR,
	Official:  false,
	Notes:     "Tesseract's tessdata_fast 4.1.0 script models (Apache-2.0), fetched on demand for document_ocr.",
	Pin: &Pin{
		Revision: "65727574dfcd264acbb0c3e07860e4e9e9b22185",
		BaseURL:  "https://raw.githubusercontent.com/" + TessdataFastRepo + "/65727574dfcd264acbb0c3e07860e4e9e9b22185",
		Files: []PinnedFile{
			{Path: "script/Arabic.traineddata", Bytes: 9311038,
				SHA256: "47c262ac4e843c024df87ffa1363b77a821a843c069c71ef7650f4d2ecfea1e8"},
			{Path: "script/Armenian.traineddata", Bytes: 7879909,
				SHA256: "e94e3cfcb79dc6bc24b819ce01340b690f2aabe4c5ac295545ae1bca5d7472f4"},
			{Path: "script/Bengali.traineddata", Bytes: 5712242,
				SHA256: "d04c2064bd0ffcd18488c4048d40d88ab056185f3462c2337b52f9e6c51158e5"},
			{Path: "script/Canadian_Aboriginal.traineddata", Bytes: 7180795,
				SHA256: "ec11e31e65541a2040982ee8a08514278f850127908df71553f0958cd02609f8"},
			{Path: "script/Cherokee.traineddata", Bytes: 4231619,
				SHA256: "76df30145426bde13c7ab07a2cb9a27b415a7d703472424801b35c3c3da3aab7"},
			{Path: "script/Cyrillic.traineddata", Bytes: 29252466,
				SHA256: "a80325ebb1c7aa2dca5002ec05b15052a51dcb2e9e65373c264a3df7ac284358"},
			{Path: "script/Devanagari.traineddata", Bytes: 17943870,
				SHA256: "3bbb87c1de2a6a2ef0a97dc041e6eea2723a1c22d638f5e38157a5cd441c12b7"},
			{Path: "script/Ethiopic.traineddata", Bytes: 9074416,
				SHA256: "4856c53285f9dd28dd130228316cdd506a5172505c99f367d63d84e6467c15f4"},
			{Path: "script/Fraktur.traineddata", Bytes: 10915632,
				SHA256: "ac985c3020e42986f8a19e07f1c1b501d47ed7d8e96de69f685bb907aa8db55c"},
			{Path: "script/Georgian.traineddata", Bytes: 6949865,
				SHA256: "8da5cc7e2af2c8da04d126ab1f2604904305e548bc3fc20d3c0bae6d519ca013"},
			{Path: "script/Greek.traineddata", Bytes: 3041556,
				SHA256: "52e201e4a22336d89ee202b579aca04f15ffd6e5e6af1fe164498f2c7de898e6"},
			{Path: "script/Gujarati.traineddata", Bytes: 5017100,
				SHA256: "94643022c1dc06a6b1a96f7dbc3bcc77f455b38e6843cc115a4c9c460b0b32aa"},
			{Path: "script/Gurmukhi.traineddata", Bytes: 4213597,
				SHA256: "34c36da0fc59198f009ebbe8646708255fa3ca2553e62645a7b584cbceca8b24"},
			{Path: "script/Hangul_vert.traineddata", Bytes: 4725034,
				SHA256: "8fbbd6366654e043038bbcf78b2b8e031c64c2b65f133593d097edee6c7e2ea8"},
			{Path: "script/Hangul.traineddata", Bytes: 4845849,
				SHA256: "cae2d12b8cdac6b62643b65915bec347e80970adc55228ca42ac1ec8909b198a"},
			{Path: "script/HanS_vert.traineddata", Bytes: 5429738,
				SHA256: "b555a9a60c88e5e27ddd34541d055200c05492d1f184732518a721faf1db979b"},
			{Path: "script/HanS.traineddata", Bytes: 5972903,
				SHA256: "d03c250faaa48015bfde2213f3d7d5bda82bcee93422fceac5906a32073d9152"},
			{Path: "script/HanT_vert.traineddata", Bytes: 5448267,
				SHA256: "ec4e31daec478e61216b1d9f34e0441fc1fcefacfe4fb28f1ea8bcfcc97ba302"},
			{Path: "script/HanT.traineddata", Bytes: 5448070,
				SHA256: "29588327f0d830b5c112209045d826b9be014138266b20f2b08843aac2730fbb"},
			{Path: "script/Hebrew.traineddata", Bytes: 4866337,
				SHA256: "4f4f3405404a61e21e5c16316ed5c3052ddb3572a75e0b3dc3fe118b12dea76e"},
			{Path: "script/Japanese_vert.traineddata", Bytes: 6445356,
				SHA256: "1d463a4811f3ae174e11b3f87773e07130c1c6aca042f31f7a90c41d939f1538"},
			{Path: "script/Japanese.traineddata", Bytes: 5877645,
				SHA256: "c176cdca0c17e9be97bda854ac91baea90b92e2813b45749ad05e409477ba8b2"},
			{Path: "script/Kannada.traineddata", Bytes: 6777791,
				SHA256: "1252172cbbe99d27e5e52009a0e79444d555329b82e0b88d6e1f2a601a1b7d86"},
			{Path: "script/Khmer.traineddata", Bytes: 4476365,
				SHA256: "330366535d1155974af0f99630df3060c9a98c657b1e4b29f458125f1d10e44a"},
			{Path: "script/Lao.traineddata", Bytes: 10103937,
				SHA256: "6bdca344c3ed9ecf405e2f8b5eb0af9832c1de7d92af2f4891867549c1d1fb63"},
			{Path: "script/Malayalam.traineddata", Bytes: 9007302,
				SHA256: "822ad7ed9fb2eba0b0da5c788ed8710eba651d91777fc659ce0d700cfb99be06"},
			{Path: "script/Myanmar.traineddata", Bytes: 7840489,
				SHA256: "78f95e6033824b7c197a099e9fdef55e0742146619d822c9b8d81f933f2b27df"},
			{Path: "script/Oriya.traineddata", Bytes: 5744929,
				SHA256: "39fc17f9e30eee261ac08b2d9d3c82bf28e69cff7c38d0a3aea19f1023410ade"},
			{Path: "script/Sinhala.traineddata", Bytes: 4782577,
				SHA256: "ce202bfbdc65e677abd76f5349f4765296b3d2b01c87608f49154ad7fa12ec4a"},
			{Path: "script/Syriac.traineddata", Bytes: 5803220,
				SHA256: "a89964433928a74de707e6fb61df1e933660de19c969f5301e8195e0219ea9bc"},
			{Path: "script/Tamil.traineddata", Bytes: 7089032,
				SHA256: "6857e222c6977986915bd073f669c2cde33039dedde26603ed6be87b0a8d62bb"},
			{Path: "script/Telugu.traineddata", Bytes: 6485174,
				SHA256: "615dd18355098058d77a45ee3bacc5d36aca574880c1b6099011f2aca09f2d1c"},
			{Path: "script/Thaana.traineddata", Bytes: 6048323,
				SHA256: "aae14e808cc783e00fe37af8afa67ff09c8d01b454065b7633e014eb2b5b6e2d"},
			{Path: "script/Thai.traineddata", Bytes: 4249016,
				SHA256: "d9e34be94556ddb65fd7e04892ff606e1a5b696d0eb422b5a7efc6859943702d"},
			{Path: "script/Tibetan.traineddata", Bytes: 5699531,
				SHA256: "0c869d52597d5b03eb6f52c5d0c217514e883259ead05765c0329e5a536538ae"},
			{Path: "script/Vietnamese.traineddata", Bytes: 1667794,
				SHA256: "30546eea618459e79761f962391e6879778b026ad7b4459a11664a1b654758bd"},
		},
	},
}

// TessdataScript returns the pinned file of tessdata_fast's script model
// named name (for example "Cyrillic" or "HanS").
func TessdataScript(name string) (PinnedFile, bool) {
	for _, f := range TessdataFastScripts.Pin.Files {
		if f.Path == "script/"+name+".traineddata" {
			return f, true
		}
	}
	return PinnedFile{}, false
}

// FetchPinnedFile returns where one pinned file of m is cached, fetching it
// over HTTPS at the pinned revision first when it is missing or does not
// match its SHA-256. The download is checked before it is moved into place.
func (c Cache) FetchPinnedFile(ctx context.Context, m Model, f PinnedFile) (string, error) {
	if m.Pin == nil {
		return "", fmt.Errorf("%s has no pin", m.ID)
	}
	dst := c.PinnedPath(m, f)
	if got, err := fileSHA256(dst); err == nil && got == f.SHA256 {
		return dst, nil
	}
	if err := PreflightDisk(c.Dir, f.Bytes); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := fetchOne(ctx, m.Pin.URL(m.Repo, f), dst, f, nil); err != nil {
		return "", fmt.Errorf("fetch %s %s: %w", m.ID, strings.TrimPrefix(f.Path, "script/"), err)
	}
	return dst, nil
}
