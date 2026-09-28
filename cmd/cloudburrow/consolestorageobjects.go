package main

// Cloud Storage objects in the console, past upload and delete (#790): an
// object's own page with its metadata, and Copy, Move or rename, Edit
// metadata, Edit storage class and, on objects checked in a bucket, Compose.
//
// Every one goes to the storage server's own API, the calls an application
// makes: Copy and Edit storage class through the official client's Copier
// (objects.rewrite), Move through ObjectHandle.Move (objects.move) and Compose
// through the Composer (objects.compose); Edit metadata is objects.patch on the
// JSON API, since the client cannot remove one custom key. A refusal is the
// API's own message.
//
// Nothing is replaced unless asked for. Copy, Move and Compose send
// ifGenerationMatch=0, so a destination that exists is refused with the API's
// precondition failure; the form's "Replace" checkbox drops the condition,
// and the console asks for the destination's name back before sending it.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// objectPage is the first segment of an object's page path, [objectPage,
// bucket, object name]. An object and a folder can share a name ("logs" and
// "logs/"), so an object cannot be addressed in the folders' path space; a
// bucket name cannot begin with an underscore, so this segment is never a
// bucket. Google's console addresses an object's page as _details/ too.
const objectPage = "_details"

// objectPath is the page path of one object.
func objectPath(bucket, name string) []string {
	return []string{objectPage, bucket, name}
}

// objectJSON is an object as objects.list returns it, the fields the console
// shows and prefills its forms from.
type objectJSON struct {
	Name               string            `json:"name"`
	Size               string            `json:"size"`
	ContentType        string            `json:"contentType"`
	CacheControl       string            `json:"cacheControl"`
	ContentDisposition string            `json:"contentDisposition"`
	StorageClass       string            `json:"storageClass"`
	Updated            string            `json:"updated"`
	Metadata           map[string]string `json:"metadata"`
}

func (o objectJSON) meta() objectMeta {
	return objectMeta{
		Name: o.Name, ContentType: o.ContentType, CacheControl: o.CacheControl,
		ContentDisposition: o.ContentDisposition, StorageClass: o.StorageClass, Metadata: o.Metadata,
	}
}

// objectMeta is what the action forms are prefilled from, read from a listing
// row or from the object itself.
type objectMeta struct {
	Name, ContentType, CacheControl, ContentDisposition, StorageClass string
	Metadata                                                          map[string]string
}

func attrsMeta(a *storage.ObjectAttrs) objectMeta {
	return objectMeta{
		Name: a.Name, ContentType: a.ContentType, CacheControl: a.CacheControl,
		ContentDisposition: a.ContentDisposition, StorageClass: a.StorageClass, Metadata: a.Metadata,
	}
}

// bucketNamePattern is the Create form's bucket name rule.
const bucketNamePattern = `^[a-z0-9][a-z0-9._\-]{1,61}[a-z0-9]$`

// objectStorageClasses are the classes Edit storage class offers: the ones the
// storage server accepts that are not legacy.
const objectStorageClasses = "STANDARD, NEARLINE, COLDLINE or ARCHIVE"

// replaceField is the checkbox that lifts ifGenerationMatch=0.
func replaceField() console.Field {
	return console.Field{
		Name: "replace", Label: "Replace the destination if it exists", Type: "checkbox",
		Help:        "Off, an existing destination is refused and left as it is.",
		Confirm:     "The object at the destination is replaced. In a bucket without versioning its bytes and metadata are gone.",
		ConfirmWith: "destination",
	}
}

// objectActions are what can be done to one object, with forms prefilled
// from it. One list serves the row's menu and the object's page, and the
// action route checks against it.
func objectActions(bucket string, o objectMeta) []console.Action {
	return []console.Action{
		{ID: "copy", Label: "Copy", Fields: objectCopyFields(bucket, o.Name)},
		{ID: "move", Label: "Move or rename", Fields: objectMoveFields(o.Name)},
		{ID: "editmetadata", Label: "Edit metadata", Fields: objectMetadataFields(o)},
		{ID: "storageclass", Label: "Edit storage class", Fields: objectStorageClassFields(o.StorageClass)},
	}
}

func objectCopyFields(bucket, name string) []console.Field {
	return []console.Field{
		{Name: "bucket", Label: "Destination bucket", Type: "text", Required: true, Default: bucket,
			Pattern: bucketNamePattern, Help: "The bucket the copy is made in; this one or another."},
		{Name: "destination", Label: "Destination name", Type: "text", Required: true, Default: name,
			Help: "The copy's full object name, folders included."},
		replaceField(),
	}
}

func objectMoveFields(name string) []console.Field {
	return []console.Field{
		{Name: "destination", Label: "New name", Type: "text", Required: true, Default: name,
			Help: "The full object name in this bucket, folders included. A move stays in its bucket."},
		replaceField(),
	}
}

func objectMetadataFields(o objectMeta) []console.Field {
	return []console.Field{
		{Name: "contentType", Label: "Content-Type", Type: "text", Default: o.ContentType,
			Help: "Empty removes it."},
		{Name: "cacheControl", Label: "Cache-Control", Type: "text", Default: o.CacheControl,
			Help: "Optional, such as no-cache or public, max-age=3600."},
		{Name: "contentDisposition", Label: "Content-Disposition", Type: "text", Default: o.ContentDisposition,
			Help: "Optional, such as attachment; filename=report.pdf."},
		{Name: "metadata", Label: "Custom metadata", Type: "map", Default: console.FormatMap(o.Metadata),
			Help: "One key=value per line. A key removed here is removed from the object."},
	}
}

func objectStorageClassFields(class string) []console.Field {
	return []console.Field{
		{Name: "storageClass", Label: "Storage class", Type: "text", Required: true, Default: class,
			Pattern: `^(STANDARD|NEARLINE|COLDLINE|ARCHIVE)$`,
			Help:    "One of " + objectStorageClasses + ". Rewrites the object as a new generation with the same bytes."},
	}
}

// composeAction is Compose on a bucket or folder page, taking the objects
// checked in its listing.
func composeAction(prefix string) console.Action {
	return console.Action{
		ID: "compose", Label: "Compose", SelectionField: "sources",
		Fields: []console.Field{
			{Name: "sources", Label: "Source objects", Type: "textarea", Required: true,
				Help: "Full object names in this bucket, one per line, joined in this order; 1 to 32."},
			{Name: "destination", Label: "Destination name", Type: "text", Required: true, Default: prefix,
				Help: "The composed object's full name in this bucket, folders included."},
			{Name: "contentType", Label: "Content-Type", Type: "text",
				Help: "Optional. Empty is application/octet-stream."},
			replaceField(),
		},
	}
}

// DetailActions implements console.PathActor.
func (p storageProvider) DetailActions(ctx context.Context, _ string, path []string) []console.Action {
	if len(path) == 0 {
		return nil
	}
	if path[0] == softObjectPage {
		return p.softDeletedActions(ctx, path)
	}
	if path[0] == managedFolderPage {
		return p.managedFolderActions(ctx, path)
	}
	if path[0] != objectPage {
		prefix := ""
		if len(path) > 1 {
			prefix = strings.Join(path[1:], "/") + "/"
		}
		actions := []console.Action{composeAction(prefix), createManagedFolderAction(path[0], prefix)}
		// A bucket's own page also offers Lock retention policy, while its
		// policy is there and unlocked (#789).
		if len(path) == 1 {
			if b, err := p.readBucket(ctx, path[0]); err == nil {
				actions = append(actions, lockRetentionAction(b)...)
			}
		}
		return actions
	}
	if len(path) != 3 {
		return nil
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	attrs, err := c.Bucket(path[1]).Object(path[2]).Attrs(ctx)
	if err != nil {
		// An object that cannot be read has nothing that can be done to it;
		// its page says why.
		return nil
	}
	return objectActions(path[1], attrsMeta(attrs))
}

// ActAt implements console.PathActor.
func (p storageProvider) ActAt(ctx context.Context, _ string, path []string, action string, values map[string]string) error {
	c, err := p.storageClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if action == "compose" {
		if len(path) == 0 || path[0] == objectPage {
			return errors.New("compose is made on a bucket's page")
		}
		return composeObjects(ctx, c.Bucket(path[0]), values)
	}
	switch action {
	case "lockretention":
		if len(path) != 1 {
			return errors.New("a retention policy is locked on its bucket's page")
		}
		return lockRetention(ctx, c, path[0])
	case "restore":
		return restoreObject(ctx, c, path)
	case "createmanagedfolder":
		return p.createManagedFolder(ctx, path, values)
	case "deletemanagedfolder":
		return p.deleteManagedFolder(ctx, path)
	}
	if len(path) != 3 || path[0] != objectPage {
		return fmt.Errorf("%s acts on one object", action)
	}
	src := c.Bucket(path[1]).Object(path[2])
	switch action {
	case "copy":
		dst, err := destination(c, strings.TrimSpace(values["bucket"]), values)
		if err != nil {
			return err
		}
		_, err = dst.CopierFrom(src).Run(ctx)
		return err
	case "move":
		name := strings.TrimSpace(values["destination"])
		if name == "" {
			return errors.New("name the object's new name")
		}
		dest := storage.MoveObjectDestination{Object: name}
		if values["replace"] != "true" {
			dest.Conditions = &storage.Conditions{DoesNotExist: true}
		}
		_, err := src.Move(ctx, dest)
		return err
	case "editmetadata":
		return p.editObjectMetadata(ctx, path[1], path[2], values)
	case "storageclass":
		return rewriteStorageClass(ctx, src, values["storageClass"])
	}
	return fmt.Errorf("%s is not an object action", action)
}

// destination is the object a copy or compose writes, refused if it exists
// unless the form asked to replace it.
func destination(c *storage.Client, bucket string, values map[string]string) (*storage.ObjectHandle, error) {
	name := strings.TrimSpace(values["destination"])
	if bucket == "" || name == "" {
		return nil, errors.New("name the destination bucket and object")
	}
	dst := c.Bucket(bucket).Object(name)
	if values["replace"] != "true" {
		dst = dst.If(storage.Conditions{DoesNotExist: true})
	}
	return dst, nil
}

func composeObjects(ctx context.Context, b *storage.BucketHandle, values map[string]string) error {
	var srcs []*storage.ObjectHandle
	for _, line := range strings.Split(values["sources"], "\n") {
		if name := strings.TrimSpace(line); name != "" {
			srcs = append(srcs, b.Object(name))
		}
	}
	if len(srcs) == 0 {
		return errors.New("name at least one source object")
	}
	name := strings.TrimSpace(values["destination"])
	if name == "" {
		return errors.New("name the composed object")
	}
	dst := b.Object(name)
	if values["replace"] != "true" {
		dst = dst.If(storage.Conditions{DoesNotExist: true})
	}
	comp := dst.ComposerFrom(srcs...)
	comp.ContentType = strings.TrimSpace(values["contentType"])
	_, err := comp.Run(ctx)
	return err
}

// editObjectMetadata is objects.patch with the four fields the form holds,
// on the JSON API an SDK calls. Custom metadata is replaced, not merged: a key
// the form no longer holds is sent as null, which is how a patch removes one
// (the Go client's ObjectAttrsToUpdate can only clear them all).
func (p storageProvider) editObjectMetadata(ctx context.Context, bucket, name string, values map[string]string) error {
	want, err := console.ParseMap(values["metadata"])
	if err != nil {
		return fmt.Errorf("custom metadata: %w", err)
	}
	objURL := fmt.Sprintf("http://%s/storage/v1/b/%s/o/%s", p.endpoint,
		urlpkg.PathEscape(bucket), urlpkg.PathEscape(name))
	var cur struct {
		Metageneration string            `json:"metageneration"`
		Metadata       map[string]string `json:"metadata"`
	}
	if err := getJSON(ctx, objURL, &cur); err != nil {
		return fmt.Errorf("cannot read %s/%s: %w", bucket, name, err)
	}
	meta := map[string]any{}
	for k := range cur.Metadata {
		meta[k] = nil // removed unless the form keeps it
	}
	for k, v := range want {
		if strings.TrimSpace(k) == "" {
			return errors.New("custom metadata: a key cannot be empty")
		}
		meta[k] = v
	}
	// An empty field is a removal, sent as null.
	field := func(key string) any {
		if v := strings.TrimSpace(values[key]); v != "" {
			return v
		}
		return nil
	}
	body := map[string]any{
		"contentType":        field("contentType"),
		"cacheControl":       field("cacheControl"),
		"contentDisposition": field("contentDisposition"),
	}
	if len(meta) > 0 {
		body["metadata"] = meta
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// Against the metageneration just read, so an edit made meanwhile by
	// something else is refused rather than overwritten.
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		objURL+"?ifMetagenerationMatch="+urlpkg.QueryEscape(cur.Metageneration), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	return nil
}

// rewriteStorageClass rewrites an object onto itself in another class,
// against the generation just read.
func rewriteStorageClass(ctx context.Context, o *storage.ObjectHandle, class string) error {
	class = strings.ToUpper(strings.TrimSpace(class))
	switch class {
	case "STANDARD", "NEARLINE", "COLDLINE", "ARCHIVE":
	default:
		return fmt.Errorf("storage class %q is not one of %s", class, objectStorageClasses)
	}
	cur, err := o.Attrs(ctx)
	if err != nil {
		return err
	}
	if cur.StorageClass == class {
		return fmt.Errorf("%s is already %s", cur.Name, class)
	}
	cp := o.If(storage.Conditions{GenerationMatch: cur.Generation}).CopierFrom(o)
	cp.StorageClass = class
	_, err = cp.Run(ctx)
	return err
}

// objectDetail is an object's page: what the server holds for it.
func (p storageProvider) objectDetail(ctx context.Context, path []string) (console.Detail, error) {
	if len(path) != 3 {
		return console.Detail{Unavailable: "an object's page is " + objectPage + "/bucket/object"}, nil
	}
	bucket, name := path[1], path[2]
	c, err := p.storageClient(ctx)
	if err != nil {
		return console.Detail{}, err
	}
	defer func() { _ = c.Close() }()
	a, err := c.Bucket(bucket).Object(name).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		// What a page opened on an object just moved or deleted shows.
		return console.Detail{Unavailable: fmt.Sprintf(
			"%s holds no object named %s: it was moved, renamed or deleted, or never existed", bucket, name)}, nil
	}
	if err != nil {
		return console.Detail{Unavailable: fmt.Sprintf("cannot read %s/%s: %v", bucket, name, err)}, nil
	}

	onOff := func(v bool) string {
		if v {
			return "On"
		}
		return "Off"
	}
	props := func(pairs ...string) []console.Property {
		var out []console.Property
		for i := 0; i+1 < len(pairs); i += 2 {
			if pairs[i+1] != "" {
				out = append(out, console.Property{Label: pairs[i], Value: pairs[i+1]})
			}
		}
		return out
	}
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	size := strconv.FormatInt(a.Size, 10) + " bytes"
	if a.Size > 0 {
		size = formatBytes(a.Size) + " (" + size + ")"
	}

	md5 := ""
	if len(a.MD5) > 0 {
		md5 = base64.StdEncoding.EncodeToString(a.MD5)
	}
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], a.CRC32C)
	custom := make([]console.Property, 0, len(a.Metadata))
	for k, v := range a.Metadata {
		if v == "" {
			v = "(empty value)"
		}
		custom = append(custom, console.Property{Label: k, Value: v})
	}
	sort.Slice(custom, func(i, j int) bool { return custom[i].Label < custom[j].Label })
	if len(custom) == 0 {
		custom = []console.Property{{Label: "Keys", Value: "None"}}
	}
	components := ""
	if a.ComponentCount > 0 {
		components = strconv.FormatInt(a.ComponentCount, 10)
	}

	groups := []console.PropertyGroup{
		{Heading: "Object", Properties: props(
			"Size", size,
			"Content-Type", a.ContentType,
			"Content-Encoding", a.ContentEncoding,
			"Content-Language", a.ContentLanguage,
			"Cache-Control", a.CacheControl,
			"Content-Disposition", a.ContentDisposition,
			"Storage class", a.StorageClass,
			"Created", stamp(a.Created),
			"Updated", stamp(a.Updated),
			"Composite components", components,
		)},
		{Heading: "Version", Properties: props(
			"Generation", strconv.FormatInt(a.Generation, 10),
			"Metageneration", strconv.FormatInt(a.Metageneration, 10),
		)},
		{Heading: "Hashes", Properties: props(
			"MD5", md5,
			"CRC32C", base64.StdEncoding.EncodeToString(crc[:]),
		)},
		{Heading: "Custom metadata", Properties: custom},
		{Heading: "Protection", Properties: props(
			"Event-based hold", onOff(a.EventBasedHold),
			"Temporary hold", onOff(a.TemporaryHold),
			"Retention expires", stamp(a.RetentionExpirationTime),
		)},
	}

	// The trail is the bucket and its folders, not the page path, whose
	// first segment is only how the page is addressed.
	trail := []console.Crumb{{Label: bucket, Path: []string{bucket}}}
	parts := strings.Split(name, "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "" {
			break // "a//b" has no folder page past the empty segment
		}
		trail = append(trail, console.Crumb{Label: parts[i], Path: append([]string{bucket}, parts[:i+1]...)})
	}
	leaf := parts[len(parts)-1]
	if leaf == "" {
		leaf = name // a folder placeholder, "a/", is named in full
	}
	trail = append(trail, console.Crumb{Label: leaf})

	return console.Detail{
		Summary: props("Bucket", bucket, "Name", name, "Size", size, "Content-Type", a.ContentType,
			"Generation", strconv.FormatInt(a.Generation, 10)),
		Sections: []console.Section{{ID: "metadata", Label: "Metadata", Kind: console.KindProperties,
			Groups: groups}},
		Trail: trail,
	}, nil
}

var _ console.PathActor = storageProvider{}
