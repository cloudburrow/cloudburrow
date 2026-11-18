package firestoreadmin

// Firestore with MongoDB compatibility (#1703): the MongoDB wire protocol
// (OP_MSG, and OP_QUERY for a legacy handshake) on a port of the front,
// for the Enterprise edition databases the Admin API keeps, as Google
// documents it (https://docs.cloud.google.com/firestore/mongodb-compatibility/docs/overview):
//
//   - a connection authenticates (mongoauth.go) and is then limited to
//     that one database, as Google's are;
//   - the commands are the ones Google's behavior differences page lists
//     (mongocmd.go), each refusing the fields Google lists as unsupported;
//     any other command is refused CommandNotFound;
//   - documents are the database's documents, read and written through
//     the front's own documents API, so the Firestore clients and the
//     console see the same documents (mql/bson.go has the mapping), and
//     the front exports them as it exports every other write.
//
// It serves TLS only, as Google requires (mongotls.go, #1738), with a
// certificate of the instance's own CA; the connection string sets
// tls=true and tlsCAFile.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

const (
	opReply      = 1
	opQuery      = 2004
	opMsg        = 2013
	maxMessage   = 48000000
	maxWireVer   = 25 // MongoDB 8.0, the newest version Google lists
	defaultBatch = 101
)

// MongoServer serves the MongoDB wire protocol for the Admin API's
// Enterprise edition databases.
type MongoServer struct {
	a        *Admin
	upstream string
	logf     func(string, ...any)

	connOnce sync.Once
	client   firestorepb.FirestoreClient
	connErr  error

	// writeMu serializes the writes, which read a collection to enforce
	// _id and unique index constraints first.
	writeMu sync.Mutex

	nextConn atomic.Int32
	cmu      sync.Mutex
	cursors  map[int64]*mongoCursor

	// smu guards sessions, the logical sessions' transactions (mongotxn.go),
	// by database and lsid.
	smu      sync.Mutex
	sessions map[string]*mongoSession
}

// mongoCursor is a query's results not yet returned.
type mongoCursor struct {
	db   string
	ns   string
	docs []bson.D
	conn int32
}

// NewMongoServer is the endpoint over a, whose documents it reads and
// writes through the Firestore documents API at upstream: the front's own
// port, so its writes are kept as every other call's are.
func (a *Admin) NewMongoServer(upstream string, logf func(string, ...any)) *MongoServer {
	s := &MongoServer{a: a, upstream: upstream, logf: logf, cursors: map[int64]*mongoCursor{}, sessions: map[string]*mongoSession{}}
	a.mu.Lock()
	a.mongoServer = s
	a.mu.Unlock()
	return s
}

func (s *MongoServer) docs() (firestorepb.FirestoreClient, error) {
	s.connOnce.Do(func() {
		conn, err := grpc.NewClient(s.upstream, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			s.connErr = err
			return
		}
		s.client = firestorepb.NewFirestoreClient(conn)
	})
	return s.client, s.connErr
}

// Serve serves l until ctx ends.
func (s *MongoServer) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		go s.serveConn(ctx, c)
	}
}

// mongoConn is one client connection's state.
type mongoConn struct {
	id   int32
	db   *mongoDB
	conv *scramConv
	// token is set by a PLAIN or MONGODB-OIDC authentication, which
	// names no database: the first command's database is the
	// connection's.
	token bool
	// principal is a token sign-in's account, or a SCRAM sign-in's user
	// credentials as an IAM principal, whose roles each command is
	// checked against (mongoiam.go, #1738).
	principal string
	// tx is the transaction the command being run is part of, if any.
	tx *mongoTxn
}

func (s *MongoServer) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	mc := &mongoConn{id: s.nextConn.Add(1)}
	defer s.dropCursors(mc.id)
	hdr := make([]byte, 16)
	for {
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		size := int(int32(binary.LittleEndian.Uint32(hdr)))
		reqID := int32(binary.LittleEndian.Uint32(hdr[4:]))
		op := int32(binary.LittleEndian.Uint32(hdr[12:]))
		if size < 16 || size > maxMessage {
			return
		}
		body := make([]byte, size-16)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		var reply []byte
		switch op {
		case opMsg:
			cmd, more, err := parseOpMsg(body)
			var resp bson.D
			if err != nil {
				resp = errorReply(err)
			} else {
				resp = s.dispatch(ctx, mc, cmd)
			}
			if more {
				continue
			}
			reply = opMsgReply(reqID, resp)
		case opQuery:
			cmd, db, err := parseOpQuery(body)
			var resp bson.D
			if err != nil {
				resp = errorReply(err)
			} else {
				cmd = append(cmd, bson.E{Key: "$db", Value: db})
				resp = s.dispatch(ctx, mc, cmd)
			}
			reply = opReplyMsg(reqID, resp)
		default:
			// OP_COMPRESSED is never negotiated, and the legacy opcodes
			// were removed from MongoDB.
			return
		}
		if reply == nil {
			return
		}
		if _, err := c.Write(reply); err != nil {
			return
		}
	}
}

func readCString(b []byte) (string, []byte, error) {
	i := 0
	for i < len(b) && b[i] != 0 {
		i++
	}
	if i == len(b) {
		return "", nil, errors.New("unterminated string")
	}
	return string(b[:i]), b[i+1:], nil
}

func parseOpMsg(b []byte) (bson.D, bool, error) {
	if len(b) < 5 {
		return nil, false, mql.Errorf(9, "FailedToParse", "OP_MSG is too short")
	}
	flags := binary.LittleEndian.Uint32(b)
	more := flags&2 != 0
	b = b[4:]
	if flags&1 != 0 {
		if len(b) < 4 {
			return nil, more, mql.Errorf(9, "FailedToParse", "OP_MSG checksum missing")
		}
		b = b[:len(b)-4]
	}
	var cmd bson.D
	var seqs []bson.E
	for len(b) > 0 {
		kind := b[0]
		b = b[1:]
		switch kind {
		case 0:
			if len(b) < 4 {
				return nil, more, mql.Errorf(9, "FailedToParse", "truncated OP_MSG body")
			}
			n := int(binary.LittleEndian.Uint32(b))
			if n < 5 || n > len(b) {
				return nil, more, mql.Errorf(9, "FailedToParse", "invalid OP_MSG body size")
			}
			if err := bson.Unmarshal(b[:n], &cmd); err != nil {
				return nil, more, mql.Errorf(9, "FailedToParse", "invalid BSON: %v", err)
			}
			b = b[n:]
		case 1:
			if len(b) < 4 {
				return nil, more, mql.Errorf(9, "FailedToParse", "truncated document sequence")
			}
			n := int(binary.LittleEndian.Uint32(b))
			if n < 4 || n > len(b) {
				return nil, more, mql.Errorf(9, "FailedToParse", "invalid document sequence size")
			}
			seq := b[4:n]
			b = b[n:]
			id, rest, err := readCString(seq)
			if err != nil {
				return nil, more, mql.Errorf(9, "FailedToParse", "invalid document sequence identifier")
			}
			var docs bson.A
			for len(rest) > 0 {
				if len(rest) < 4 {
					return nil, more, mql.Errorf(9, "FailedToParse", "truncated document in sequence")
				}
				m := int(binary.LittleEndian.Uint32(rest))
				if m < 5 || m > len(rest) {
					return nil, more, mql.Errorf(9, "FailedToParse", "invalid document size in sequence")
				}
				var d bson.D
				if err := bson.Unmarshal(rest[:m], &d); err != nil {
					return nil, more, mql.Errorf(9, "FailedToParse", "invalid BSON: %v", err)
				}
				docs = append(docs, d)
				rest = rest[m:]
			}
			seqs = append(seqs, bson.E{Key: id, Value: docs})
		default:
			return nil, more, mql.Errorf(9, "FailedToParse", "unknown OP_MSG section kind %d", kind)
		}
	}
	if len(cmd) == 0 {
		return nil, more, mql.Errorf(9, "FailedToParse", "OP_MSG has no body")
	}
	return append(cmd, seqs...), more, nil
}

func parseOpQuery(b []byte) (bson.D, string, error) {
	if len(b) < 4 {
		return nil, "", mql.Errorf(9, "FailedToParse", "OP_QUERY is too short")
	}
	ns, rest, err := readCString(b[4:])
	if err != nil || len(rest) < 12 {
		return nil, "", mql.Errorf(9, "FailedToParse", "invalid OP_QUERY")
	}
	rest = rest[8:]
	n := int(binary.LittleEndian.Uint32(rest))
	if n < 5 || n > len(rest) {
		return nil, "", mql.Errorf(9, "FailedToParse", "invalid OP_QUERY document")
	}
	var q bson.D
	if err := bson.Unmarshal(rest[:n], &q); err != nil {
		return nil, "", mql.Errorf(9, "FailedToParse", "invalid BSON: %v", err)
	}
	if inner, ok := mql.Lookup(q, "$query").(bson.D); ok {
		q = inner
	}
	db, coll, _ := strings.Cut(ns, ".")
	if coll != "$cmd" {
		return nil, "", mql.Errorf(5739101, "Location5739101", "OP_QUERY is no longer supported, except for the hello and isMaster commands")
	}
	return q, db, nil
}

func header(buf []byte, reqID int32, op int32) {
	binary.LittleEndian.PutUint32(buf[0:], uint32(len(buf)))
	var id [4]byte
	_, _ = rand.Read(id[:])
	copy(buf[4:8], id[:])
	binary.LittleEndian.PutUint32(buf[8:], uint32(reqID))
	binary.LittleEndian.PutUint32(buf[12:], uint32(op))
}

func opMsgReply(reqID int32, resp bson.D) []byte {
	raw, err := bson.Marshal(resp)
	if err != nil {
		raw, _ = bson.Marshal(errorReply(mql.Errorf(1, "InternalError", "the reply cannot be encoded: %v", err)))
	}
	buf := make([]byte, 16+4+1+len(raw))
	buf[20] = 0
	copy(buf[21:], raw)
	header(buf, reqID, opMsg)
	return buf
}

func opReplyMsg(reqID int32, resp bson.D) []byte {
	raw, err := bson.Marshal(resp)
	if err != nil {
		raw, _ = bson.Marshal(errorReply(mql.Errorf(1, "InternalError", "the reply cannot be encoded: %v", err)))
	}
	buf := make([]byte, 16+20+len(raw))
	binary.LittleEndian.PutUint32(buf[32:], 1)
	copy(buf[36:], raw)
	header(buf, reqID, opReply)
	return buf
}

// errorReply is a command's error reply.
func errorReply(err error) bson.D {
	var me *mql.Error
	if !errors.As(err, &me) {
		me = mql.Errorf(1, "InternalError", "%v", err)
	}
	out := bson.D{{Key: "ok", Value: 0.0}, {Key: "errmsg", Value: me.Msg}, {Key: "code", Value: me.Code}, {Key: "codeName", Value: me.Name}}
	var l *labeled
	if errors.As(err, &l) {
		labels := bson.A{}
		for _, x := range l.labels {
			labels = append(labels, x)
		}
		out = append(out, bson.E{Key: "errorLabels", Value: labels})
	}
	return out
}

// noAuth are the commands an unauthenticated connection may run.
var noAuth = map[string]bool{"hello": true, "ismaster": true, "ping": true, "buildinfo": true, "saslstart": true,
	"saslcontinue": true, "endsessions": true, "connectionstatus": true, "whatsmyuri": true}

func (s *MongoServer) dispatch(ctx context.Context, mc *mongoConn, cmd bson.D) bson.D {
	name := cmd[0].Key
	lname := strings.ToLower(name)
	dbName, _ := mql.Lookup(cmd, "$db").(string)
	if mc.db == nil && !mc.token && !noAuth[lname] {
		return errorReply(mql.Errorf(13, "Unauthorized", "Command %s requires authentication", name))
	}
	if !noAuth[lname] && lname != "listdatabases" {
		if mc.db == nil && mc.token {
			db, err := s.a.mongoDatabase(dbName)
			if err != nil {
				return errorReply(mql.Errorf(13, "Unauthorized", "not authorized on %s to execute command %s: no Enterprise edition database %q", dbName, name, dbName))
			}
			mc.db = db
		}
		if dbName != mc.db.id && !(dbName == "admin" && (lname == "committransaction" || lname == "aborttransaction")) {
			return errorReply(mql.Errorf(13, "Unauthorized", "not authorized on %s to execute command %s: each connection is limited to a single database, %s", dbName, name, mc.db.id))
		}
	}
	if err := checkCommonOptions(cmd); err != nil {
		return errorReply(err)
	}
	if err := s.checkMongoIAM(mc, lname, name, cmd); err != nil {
		return errorReply(err)
	}
	var resp bson.D
	var err error
	switch lname {
	case "hello", "ismaster":
		resp = s.hello(mc, cmd, lname == "hello")
	case "ping":
		resp = bson.D{}
	case "buildinfo":
		resp = bson.D{{Key: "version", Value: "8.0.0"}, {Key: "versionArray", Value: bson.A{int32(8), int32(0), int32(0), int32(0)}},
			{Key: "maxBsonObjectSize", Value: int32(mql.MaxDocSize)}}
	case "whatsmyuri":
		resp = bson.D{{Key: "you", Value: "127.0.0.1"}}
	case "connectionstatus":
		users := bson.A{}
		if mc.db != nil {
			users = append(users, bson.D{{Key: "db", Value: mc.db.id}})
		}
		resp = bson.D{{Key: "authInfo", Value: bson.D{{Key: "authenticatedUsers", Value: users}, {Key: "authenticatedUserRoles", Value: bson.A{}}}}}
	case "saslstart":
		resp, err = s.saslStart(mc, cmd, dbName)
	case "saslcontinue":
		resp, err = s.saslContinue(mc, cmd)
	case "endsessions":
		s.endSessions(mc, cmd)
		resp = bson.D{}
	default:
		tx, done, terr := s.txnFor(ctx, mc, lname, cmd)
		switch {
		case terr == errCommitted || terr == errAborted:
			resp = bson.D{}
		case terr != nil:
			err = terr
		case lname == "committransaction" || lname == "aborttransaction":
			err = mql.Errorf(251, "NoSuchTransaction", "%s needs a transaction: autocommit false, a txnNumber and an lsid.", name)
		default:
			mc.tx = tx
			resp, err = s.command(ctx, mc, lname, name, cmd)
			mc.tx = nil
			done(err != nil || mql.Lookup(resp, "writeErrors") != nil)
		}
	}
	if err != nil {
		return errorReply(err)
	}
	return append(resp, bson.E{Key: "ok", Value: 1.0})
}

// checkCommonOptions refuses the write concerns, read concerns and read
// preferences Google does not support.
func checkCommonOptions(cmd bson.D) error {
	if wc, ok := mql.Lookup(cmd, "writeConcern").(bson.D); ok {
		if w, ok := mql.LookupE(wc, "w"); ok {
			n, isNum := mql.NumberOf(w)
			if !(isNum && n == 1) && w != "majority" {
				return mql.BadValue("Only w: 'majority' and w: 1 write concerns are supported.")
			}
		}
	}
	if rc, ok := mql.Lookup(cmd, "readConcern").(bson.D); ok {
		if lv, ok := mql.LookupE(rc, "level"); ok {
			switch lv {
			case "snapshot", "majority", "linearizable":
			default:
				return mql.BadValue("Only the snapshot, majority and linearizable read concerns are supported, not %v.", lv)
			}
		}
	}
	if rp, ok := mql.Lookup(cmd, "$readPreference").(bson.D); ok {
		switch mql.Lookup(rp, "mode") {
		case nil, "primary", "primaryPreferred", "secondaryPreferred", "nearest":
		default:
			return mql.BadValue("Only the primary, primaryPreferred, secondaryPreferred and nearest read preferences are supported, not %v.", mql.Lookup(rp, "mode"))
		}
	}
	return nil
}

func (s *MongoServer) hello(mc *mongoConn, cmd bson.D, hello bool) bson.D {
	primary := "isWritablePrimary"
	if !hello {
		primary = "ismaster"
	}
	resp := bson.D{{Key: "helloOk", Value: true}, {Key: primary, Value: true},
		{Key: "maxBsonObjectSize", Value: int32(mql.MaxDocSize)}, {Key: "maxMessageSizeBytes", Value: int32(maxMessage)},
		{Key: "maxWriteBatchSize", Value: int32(100000)}, {Key: "localTime", Value: bson.NewDateTimeFromTime(time.Now())},
		{Key: "logicalSessionTimeoutMinutes", Value: int32(30)}, {Key: "connectionId", Value: mc.id},
		{Key: "minWireVersion", Value: int32(0)}, {Key: "maxWireVersion", Value: int32(maxWireVer)}, {Key: "readOnly", Value: false}}
	if mql.Truthy(mql.Lookup(cmd, "loadBalanced")) {
		resp = append(resp, bson.E{Key: "serviceId", Value: bson.NewObjectID()})
	}
	if mechs, ok := mql.Lookup(cmd, "saslSupportedMechs").(string); ok && mechs != "" {
		resp = append(resp, bson.E{Key: "saslSupportedMechs", Value: bson.A{"SCRAM-SHA-256"}})
	}
	return resp
}

func payloadOf(cmd bson.D) []byte {
	switch p := mql.Lookup(cmd, "payload").(type) {
	case bson.Binary:
		return p.Data
	case string:
		return []byte(p)
	}
	return nil
}

func (s *MongoServer) saslStart(mc *mongoConn, cmd bson.D, dbName string) (bson.D, error) {
	mech, _ := mql.Lookup(cmd, "mechanism").(string)
	payload := payloadOf(cmd)
	switch mech {
	case "SCRAM-SHA-256":
		conv, first, err := s.a.scramStart(dbName, payload)
		if err != nil {
			return nil, err
		}
		mc.conv = conv
		return bson.D{{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: false},
			{Key: "payload", Value: bson.Binary{Data: first}}}, nil
	case "PLAIN":
		parts := strings.Split(string(payload), "\x00")
		if dbName != "$external" || len(parts) != 3 || parts[1] != "access_token" || parts[2] == "" {
			return nil, authFailed()
		}
		email, ok := s.a.mongoTokenEmail(parts[2])
		if !ok {
			return nil, authFailed()
		}
		mc.token, mc.principal = true, email
		return bson.D{{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: true}, {Key: "payload", Value: bson.Binary{Data: []byte{}}}}, nil
	case "MONGODB-OIDC":
		var p bson.D
		if dbName != "$external" || bson.Unmarshal(payload, &p) != nil {
			return nil, authFailed()
		}
		jwt, _ := mql.Lookup(p, "jwt").(string)
		if jwt == "" {
			return nil, authFailed()
		}
		email, ok := s.a.mongoTokenEmail(jwt)
		if !ok {
			return nil, authFailed()
		}
		mc.token, mc.principal = true, email
		return bson.D{{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: true}, {Key: "payload", Value: bson.Binary{Data: []byte{}}}}, nil
	}
	return nil, mql.Errorf(334, "MechanismUnavailable", "Received authentication for mechanism %s which is unknown or not enabled", mech)
}

func (s *MongoServer) saslContinue(mc *mongoConn, cmd bson.D) (bson.D, error) {
	if mc.conv == nil {
		if mc.db != nil {
			return bson.D{{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: true}, {Key: "payload", Value: bson.Binary{Data: []byte{}}}}, nil
		}
		return nil, mql.Errorf(17, "ProtocolError", "No SASL session state found")
	}
	conv := mc.conv
	mc.conv = nil
	final, err := conv.finish(payloadOf(cmd))
	if err != nil {
		return nil, err
	}
	mc.db = conv.db
	mc.principal = UserCredsPrincipal(conv.db.name, conv.user)
	return bson.D{{Key: "conversationId", Value: int32(1)}, {Key: "done", Value: true}, {Key: "payload", Value: bson.Binary{Data: final}}}, nil
}

// newCursor keeps docs for getMore, and is its ID.
func (s *MongoServer) newCursor(mc *mongoConn, ns string, docs []bson.D) int64 {
	if len(docs) == 0 {
		return 0
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	id := int64(binary.LittleEndian.Uint64(b[:]) >> 1)
	if id == 0 {
		id = 1
	}
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.cursors[id] = &mongoCursor{db: mc.db.name, ns: ns, docs: docs, conn: mc.id}
	return id
}

// dropCursors closes a closed connection's cursors.
func (s *MongoServer) dropCursors(conn int32) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	for id, c := range s.cursors {
		if c.conn == conn {
			delete(s.cursors, id)
		}
	}
}

// batch is the first n documents of docs that fit a reply, and the rest.
func batch(docs []bson.D, n int) (bson.A, []bson.D) {
	if n <= 0 {
		n = len(docs)
	}
	out := bson.A{}
	size := 0
	for i, d := range docs {
		if i >= n {
			return out, docs[i:]
		}
		raw, _ := bson.Marshal(d)
		if size+len(raw) > mql.MaxDocSize && len(out) > 0 {
			return out, docs[i:]
		}
		size += len(raw)
		out = append(out, d)
	}
	return out, nil
}

func cursorReply(id int64, ns string, key string, docs bson.A) bson.D {
	return bson.D{{Key: "cursor", Value: bson.D{{Key: "id", Value: id}, {Key: "ns", Value: ns}, {Key: key, Value: docs}}}}
}

func (s *MongoServer) logError(format string, args ...any) {
	if s.logf != nil {
		s.logf("firestore mongodb: "+format, args...)
	}
}

var _ = fmt.Sprint
