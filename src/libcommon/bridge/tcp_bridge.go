package bridge

import (
	"app"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net"
	"strconv"
	"util/pool"
)

// operation codes const.
const (
	O_CONNECT  = 1
	O_RESPONSE = 2

	O_QUERY_FILE     = 3
	O_DOWNLOAD_FILE  = 4
	O_REG_STORAGE    = 5
	O_REG_FILE       = 6
	O_SYNC_STORAGE   = 7
	O_PULL_NEW_FILES = 8
	O_UPLOAD         = 9
	O_SYNC_MEMBERS   = 10
)

const HeaderSize = 18

// store operation code mapped with 2 first head bytes.
var operationHeadMap = make(map[int][]byte)

var bytesPool *pool.BytesPool

// error indicate that the operation is not support.
var OPERATION_NOT_SUPPORT_ERROR = errors.New("operation not support")
var SEND_HEAD_BYTES_ERROR = errors.New("error send head bytes")
var SEND_BODY_BYTES_ERROR = errors.New("error send head bytes")
var READ_ERROR = errors.New("error read bytes")
var FILE_NOT_FOUND_ERROR = errors.New("file not found")
var INTERNAL_SERVER_ERROR = errors.New("internal server error")
var DOWNLOAD_FILE_ERROR = errors.New("error download file")

// init operations
func init() {
	operationHeadMap[O_CONNECT] = []byte{1, 1}
	operationHeadMap[O_RESPONSE] = []byte{1, 2}
	operationHeadMap[O_UPLOAD] = []byte{1, 3}
	operationHeadMap[O_QUERY_FILE] = []byte{1, 4}
	operationHeadMap[O_DOWNLOAD_FILE] = []byte{1, 5}
	operationHeadMap[O_REG_STORAGE] = []byte{1, 6}
	operationHeadMap[O_REG_FILE] = []byte{1, 7}
	operationHeadMap[O_SYNC_STORAGE] = []byte{1, 8}
	operationHeadMap[O_PULL_NEW_FILES] = []byte{1, 9}
	operationHeadMap[O_SYNC_MEMBERS] = []byte{1, 10}

	bytesPool = pool.NewBytesPool(50)
}

// SendReceiveCloser
type SendReceiver interface {
	// client send request.
	SendRequest(operation int, meta interface{}, bodyLen uint64, bodyWriterHandler func(out io.WriteCloser) error) error
	// server receive request, it will block till get a new request from client.
	ReceiveRequest(requestHandler func(request *Meta, in io.ReadCloser) error) error
	// server send response.
	SendResponse(meta interface{}, bodyLen uint64, bodyWriterHandler func(out io.WriteCloser) error) error
	// client receive response from server.
	ReceiveResponse(responseHandler func(response *Meta, in io.ReadCloser) error) error
	// send validate request
	ValidateConnection() error
	GetConn() net.Conn
}

// include a tcp interact request meta data:
// operation bytes, meta length bytes, body length bytes and meta body bytes.
type Meta struct {
	Operation  int
	metaLength uint64
	BodyLength uint64
	MetaBody   []byte
	Err        error
}

type Bridge struct {
	connection net.Conn
	UUID       string
}

func (bridge *Bridge) Close() {
	Close(bridge.connection)
}

func (bridge *Bridge) GetConn() net.Conn {
	return bridge.connection
}

func (bridge *Bridge) SendRequest(operation int, meta interface{}, bodyLen uint64, bodyWriterHandler func(out io.WriteCloser) error) error {
	request, e2 := CreateMeta(operation, meta, bodyLen)
	if e2 != nil {
		return e2
	}
	tmpBuf, _ := MakeBytes(8, false, 0, false)
	defer RecycleBytes(tmpBuf)

	var headerBuff bytes.Buffer
	headerBuff.Write(operationHeadMap[request.Operation])
	metaLenBytes := convertLen2Bytes(request.metaLength, &tmpBuf)
	headerBuff.Write(*metaLenBytes)
	bodyLenBytes := convertLen2Bytes(request.BodyLength, &tmpBuf)
	headerBuff.Write(*bodyLenBytes)

	headerBuff.Write(request.MetaBody)
	len1, e1 := bridge.connection.Write(headerBuff.Bytes())
	if e1 != nil {
		Close(bridge.connection)
		return e1
	}
	if len1 != headerBuff.Len() {
		Close(bridge.connection)
		return SEND_HEAD_BYTES_ERROR
	}
	app.UpdateIOOUT(int64(headerBuff.Len()))
	if request.BodyLength > 0 {
		// write request body bytes using custom writer handler.
		err := bodyWriterHandler(bridge.connection)
		if err != nil {
			Close(bridge.connection)
			return err
		}
	}
	return nil
}

func (bridge *Bridge) ReceiveResponse(responseHandler func(response *Meta, in io.Reader) error) error {
	operation, metaSize, bodySize, metaBodyBytes, e1 := readHeadBytes(bridge.connection)
	if e1 != nil {
		return e1
	}
	response := &Meta{
		Operation:  operation,
		metaLength: metaSize,
		BodyLength: bodySize,
		MetaBody:   metaBodyBytes,
	}
	return responseHandler(response, bridge.connection)
}

func (bridge *Bridge) ReceiveRequest(requestHandler func(request *Meta, in io.ReadCloser) error) error {
	operation, _, bodySize, metaBodyBytes, e1 := readHeadBytes(bridge.connection)
	if e1 != nil {
		return e1
	}
	request := &Meta{
		Operation:  operation,
		BodyLength: bodySize,
		MetaBody:   metaBodyBytes,
	}
	return requestHandler(request, bridge.connection)
}

func (bridge *Bridge) SendResponse(meta interface{}, bodyLen uint64, bodyWriterHandler func(out io.WriteCloser) error) error {
	response, e2 := CreateMeta(O_RESPONSE, meta, bodyLen)
	if e2 != nil {
		return e2
	}
	tmpBuf, _ := MakeBytes(8, false, 0, false)
	defer RecycleBytes(tmpBuf)
	//logger.Debug(string(response.MetaBody))
	var headerBuff bytes.Buffer
	headerBuff.Write(operationHeadMap[response.Operation])
	metaLenBytes := convertLen2Bytes(response.metaLength, &tmpBuf)
	headerBuff.Write(*metaLenBytes)
	bodyLenBytes := convertLen2Bytes(response.BodyLength, &tmpBuf)
	headerBuff.Write(*bodyLenBytes)

	headerBuff.Write(response.MetaBody)
	len1, e1 := bridge.connection.Write(headerBuff.Bytes())
	if e1 != nil {
		Close(bridge.connection)
		return e1
	}
	if len1 != headerBuff.Len() {
		Close(bridge.connection)
		return SEND_HEAD_BYTES_ERROR
	}
	if response.BodyLength > 0 {
		// write request body bytes using custom writer handler.
		err := bodyWriterHandler(bridge.connection)
		if err != nil {
			Close(bridge.connection)
			return err
		}
	}
	return nil
}

func (bridge *Bridge) ValidateConnection(secret string) (bool, error) {
	var sec = app.SECRET
	var isNew = false
	if secret != "" {
		sec = secret
	}
	validateMeta := &OperationValidationRequest{
		Secret: sec,
		UUID:   app.UUID,
	}
	// send validate request
	e1 := bridge.SendRequest(O_CONNECT, validateMeta, 0, nil)
	if e1 != nil {
		return isNew, e1
	}
	e4 := bridge.ReceiveResponse(func(response *Meta, in io.Reader) error {
		if response.Err != nil {
			return response.Err
		}
		var validateResp = &OperationValidationResponse{}
		//logger.Debug(string(response.MetaBody))
		e3 := json.Unmarshal(response.MetaBody, validateResp)
		if e3 != nil {
			return e3
		}
		if validateResp.Status != STATUS_OK {
			return errors.New("error connect to server, server response status:" + strconv.Itoa(validateResp.Status))
		}
		bridge.UUID = validateResp.UUID
		if validateResp.IsNew {
			isNew = true
		}
		// connect success
		return nil
	})
	if e4 != nil {
		return isNew, e4
	}
	return isNew, nil
}

func NewBridge(conn net.Conn) *Bridge {
	return &Bridge{connection: conn}
}

func Close(closer io.Closer) error {
	if closer != nil {
		return closer.Close()
	}
	return nil
}

func convertLen2Bytes(len uint64, buffer *[]byte) *[]byte {
	binary.BigEndian.PutUint64(*buffer, uint64(len))
	return buffer
}

// 通用字节读取函数，如果读取结束/失败自动关闭连接
// ioinout bool, true is in and false is out
func ReadBytes(buff []byte, len int, conn io.ReadCloser, md hash.Hash) (int, error) {
	read := 0
	for {
		if read >= len {
			break
		}
		l, e := conn.Read(buff[read:len])
		if l == 0 || e == io.EOF {
			Close(conn)
			return 0, READ_ERROR
		}
		if l <= len {
			read += l
			continue
		}
	}
	if md != nil {
		_, e1 := md.Write(buff[0:len])
		if e1 != nil {
			return 0, e1
		}
	}
	app.UpdateIOIN(int64(len))
	return len, nil
}

// read 18 head bytes.
func readHeadBytes(reader io.ReadCloser) (int, uint64, uint64, []byte, error) {
	headerBytes, _ := MakeBytes(HeaderSize, false, 0, false) // meta header size
	defer RecycleBytes(headerBytes)
	// read header meta data
	len, e := ReadBytes(headerBytes, HeaderSize, reader, nil)
	if e == nil && len == HeaderSize {
		operation := retrieveOperation(headerBytes[0:2])
		// read meta and body size
		bMetaSize := headerBytes[2:10]
		bBodySize := headerBytes[10:18]
		metaSize := binary.BigEndian.Uint64(bMetaSize)
		bodySize := binary.BigEndian.Uint64(bBodySize)
		metaBodyBytes, e1 := readMetaBytes(int(metaSize), reader)
		if e1 != nil {
			return 0, 0, 0, nil, e1
		}
		return operation, metaSize, bodySize, metaBodyBytes, nil
	}
	return 0, 0, 0, nil, READ_ERROR
}

// 读取meta字节信息
func readMetaBytes(metaSize int, reader io.ReadCloser) ([]byte, error) {
	tmp, me := MakeBytes(uint64(metaSize), true, 5242880, true) //5MB
	if me != nil {
		return nil, me
	}
	len, e := ReadBytes(tmp, metaSize, reader, nil)
	if e != nil && e != io.EOF {
		return nil, e
	}
	if (e == nil || e == io.EOF) && len == metaSize {
		return tmp, nil
	}
	//should never happen, mark as broken connection
	return nil, READ_ERROR
}

// retrieve operation code from operation head bytes.
// return 0 if no operation code matches.
func retrieveOperation(op []byte) int {
	for k, v := range operationHeadMap {
		if bytes.Compare(op, v) == 0 {
			return k
		}
	}
	return 0
}

// create a new tcp request using given data
// operation: operation code, such as 'O_CONNECT'
// meta     : meta object
// bodyLen  : request body length
// if create success, it returns a *Request, or else returns with error
func CreateMeta(operation int, meta interface{}, bodyLen uint64) (*Meta, error) {
	// operation bytes not found
	if operationHeadMap[operation] == nil {
		return nil, OPERATION_NOT_SUPPORT_ERROR
	}

	metaBodyBytes, e := json.Marshal(meta)
	if e != nil {
		return nil, e
	}

	metaLen := uint64(len(metaBodyBytes))

	return &Meta{
		operation,
		metaLen,
		bodyLen,
		metaBodyBytes,
		nil,
	}, nil
}

// dynamic: whether apply size is dynamic or unknown before,
// and we don't cache those.
func MakeBytes(len uint64, dangerCheck bool, max uint64, dynamic bool) ([]byte, error) {
	if dangerCheck && len > max {
		return nil, errors.New("cannot create bytes: system protection")
	}
	if !dynamic {
		return bytesPool.Apply(int(len)), nil
	}
	return make([]byte, len), nil
}

// recycle bytes buffer.
func RecycleBytes(buffer []byte) {
	bytesPool.Recycle(buffer)
}
