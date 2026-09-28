package bigqueryfront

import (
	"bytes"
	"errors"
	"io"

	"github.com/apache/arrow/go/v15/arrow/ipc"
)

// The Storage Read API's Arrow messages as BigQuery frames them (#1046).
//
// BigQuery's ReadSession.arrow_schema.serialized_schema is the IPC
// message of the schema, and each ReadRowsResponse's
// arrow_record_batch.serialized_record_batch the IPC message of a record
// batch (https://cloud.google.com/bigquery/docs/reference/storage/rpc/google.cloud.bigquery.storage.v1#arrowrecordbatch):
// a client reads a batch after the schema, as the Go client does (its
// ArrowRecordBatch reader is the schema's bytes, then the batch's) and
// pyarrow's read_record_batch(batch, schema) does.
//
// The pinned emulator writes each as a whole IPC stream instead
// (server/storage_handler.go): the schema as the schema, an empty record
// batch and the end-of-stream marker; each batch as the schema, the batch
// and the marker. Read after the schema, as the Go client reads it, a
// batch is then the schema's empty batch, and the stream ends before its
// rows: measured, Table.Read with Client.EnableStorageReadClient
// (cloud.google.com/go/bigquery v1.85.0) panicked "index out of range
// [0] with length 0" on a table of one row.
//
// So the front sends the schema's message alone (storageread.go), and
// writes a batch's messages but the schema and the marker (since #1095 the
// front writes the batches itself, storagerows.go). A text it cannot read
// as an IPC stream is sent as it came.

// arrowMessages splits an Arrow IPC stream into the bytes of each of its
// messages, the end-of-stream marker left out, and tells which is the
// schema; ok is false when b is not such a stream.
func arrowMessages(b []byte) (msgs [][]byte, schema []bool, ok bool) {
	rd := bytes.NewReader(b)
	mr := ipc.NewMessageReader(rd)
	defer mr.Release()
	at := 0
	for {
		m, err := mr.Message()
		if errors.Is(err, io.EOF) {
			return msgs, schema, len(msgs) > 0
		}
		if err != nil {
			return nil, nil, false
		}
		end := len(b) - rd.Len()
		msgs = append(msgs, b[at:end])
		schema = append(schema, m.Type() == ipc.MessageSchema)
		at = end
	}
}

// arrowSchemaMessage is the schema message of an emulator's schema
// stream, or b as it is.
func arrowSchemaMessage(b []byte) []byte {
	msgs, schema, ok := arrowMessages(b)
	if !ok || !schema[0] {
		return b
	}
	return msgs[0]
}

// arrowBatchMessages is the messages of an emulator's batch stream but
// its schema, or b as it is.
func arrowBatchMessages(b []byte) []byte {
	msgs, schema, ok := arrowMessages(b)
	if !ok {
		return b
	}
	var out []byte
	for i, m := range msgs {
		if !schema[i] {
			out = append(out, m...)
		}
	}
	return out
}
