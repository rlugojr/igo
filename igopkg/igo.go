package igo

import (
    "fmt"
    "io/ioutil"
    "encoding/json"
    "encoding/hex"
    "crypto/sha256"
    "crypto/hmac"
    zmq "github.com/alecthomas/gozmq"
    uuid "github.com/nu7hatch/gouuid"
    "go/token"
    "github.com/sbinet/go-eval/pkg/eval"

)

type MsgHeader struct  {
    Msg_id string `json:"msg_id"`
    Username string `json:"username"`
    Session string `json:"session"`
    Msg_type string `json:"msg_type"`
}

type ComposedMsg struct {
    Header MsgHeader
    Parent_header MsgHeader
    Metadata map[string]interface{}
    Content interface{}
}

type ConnectionInfo struct {
    Signature_scheme string
    Transport string
    Stdin_port int
    Control_port int
    IOPub_port int
    HB_port int
    Shell_port int
    Key string
    IP string
}

type SocketGroup struct {
    Shell_socket *zmq.Socket
    Stdin_socket *zmq.Socket
    IOPub_socket *zmq.Socket
    Key []byte
}



func PrepareSockets(conn_info ConnectionInfo) (sg SocketGroup) {
    context, _ := zmq.NewContext()
    sg.Shell_socket, _ = context.NewSocket(zmq.ROUTER)
    sg.Stdin_socket, _ = context.NewSocket(zmq.ROUTER)
    sg.IOPub_socket, _ = context.NewSocket(zmq.PUB)

    address := fmt.Sprintf("%v://%v:%%v", conn_info.Transport, conn_info.IP)

    sg.Shell_socket.Bind(fmt.Sprintf(address, conn_info.Shell_port))
    sg.Stdin_socket.Bind(fmt.Sprintf(address, conn_info.Stdin_port))
    sg.IOPub_socket.Bind(fmt.Sprintf(address, conn_info.IOPub_port))

    // Message signing key
    sg.Key = []byte(conn_info.Key)

    // Start the heartbeat device
    HB_socket, _ := context.NewSocket(zmq.REP)
    HB_socket.Bind(fmt.Sprintf(address, conn_info.HB_port))
    go zmq.Device(zmq.FORWARDER, HB_socket, HB_socket)
    return
}

type InvalidSignatureError struct {}
func (e *InvalidSignatureError) Error() string {
    return "A message had an invalid signature"
}

func WireMsgToComposedMsg(msgparts [][]byte, signkey []byte) (msg ComposedMsg,
                            identities [][]byte, err error) {
    i := 0
    for string(msgparts[i]) != "<IDS|MSG>" {
        i++
    }
    identities = msgparts[:i]
    // msgparts[i] is the delimiter

    // Validate signature
    if len(signkey) != 0 {
        mac := hmac.New(sha256.New, signkey)
        for _, msgpart := range msgparts[i+2:i+6] {
            mac.Write(msgpart)
        }
        signature := make([]byte, hex.DecodedLen(len(msgparts[i+1])))
        hex.Decode(signature, msgparts[i+1])
        if !hmac.Equal(mac.Sum(nil), signature) {
            return msg, nil, &InvalidSignatureError{}
        }
    }
    json.Unmarshal(msgparts[i+2], &msg.Header)
    json.Unmarshal(msgparts[i+3], &msg.Parent_header)
    json.Unmarshal(msgparts[i+4], &msg.Metadata)
    json.Unmarshal(msgparts[i+5], &msg.Content)
    return
}

func (msg ComposedMsg) ToWireMsg(signkey []byte) (msgparts [][]byte) {
    msgparts = make([][]byte, 5)
    header, _ := json.Marshal(msg.Header)
    msgparts[1] = header
    parent_header, _ := json.Marshal(msg.Parent_header)
    msgparts[2] = parent_header
    if msg.Metadata == nil {
        msg.Metadata = make(map[string]interface{})
    }
    metadata, _ := json.Marshal(msg.Metadata)
    msgparts[3] = metadata
    content, _ := json.Marshal(msg.Content)
    msgparts[4] = content

    // Sign the message
    if len(signkey) != 0 {
        mac := hmac.New(sha256.New, signkey)
        for _, msgpart := range msgparts[1:] {
            mac.Write(msgpart)
        }
        msgparts[0] = make([]byte, hex.EncodedLen(mac.Size()))
        hex.Encode(msgparts[0], mac.Sum(nil))
    }
    return
}

type MsgReceipt struct {
    Msg ComposedMsg
    Identities [][]byte
    Sockets SocketGroup
}

func (receipt *MsgReceipt) SendResponse(socket *zmq.Socket, msg ComposedMsg) {
    socket.SendMultipart(receipt.Identities, zmq.SNDMORE)
    socket.Send([]byte("<IDS|MSG>"), zmq.SNDMORE)
    socket.SendMultipart(msg.ToWireMsg(receipt.Sockets.Key), 0)
    fmt.Println("<--", msg.Header.Msg_type)
    fmt.Println(msg.Content)
}


func HandleShellMsg(receipt MsgReceipt) {
    //fmt.Println(msg)
    switch receipt.Msg.Header.Msg_type {
        case "kernel_info_request":
            SendKernelInfo(receipt)
        case "execute_request":
            HandleExecuteRequest(receipt)
        default: fmt.Println("Other:", receipt.Msg.Header.Msg_type)
    }
}

func NewMsg(msg_type string, parent ComposedMsg) (msg ComposedMsg) {
    msg.Parent_header = parent.Header
    msg.Header.Session = parent.Header.Session
    msg.Header.Username = parent.Header.Username
    msg.Header.Msg_type = msg_type
    u, _ := uuid.NewV4()
    msg.Header.Msg_id = u.String()
    return
}

type KernelInfo struct {
    Protocol_version []int `json:"protocol_version"`
    Language string `json:"language"`
}

type KernelStatus struct {
    ExecutionState string `json:"execution_state"`
}

func SendKernelInfo(receipt MsgReceipt) {
    reply := NewMsg("kernel_info_reply", receipt.Msg)
    reply.Content = KernelInfo{[]int{4, 0}, "go"}
    receipt.SendResponse(receipt.Sockets.Shell_socket, reply)
}

var World *eval.World
var fset *token.FileSet
var ExecCounter int = 0

func RunCode(text string) (val interface{}, err error) {
    var code eval.Code
    code, err = World.Compile(fset, text)
    if err != nil {
        return nil, err
    }
    val, err = code.Run()
    return
}

type OutputMsg struct {
    Execcount int `json:"execution_count"`
    Data map[string]string `json:"data"`
    Metadata map[string]interface{} `json:"metadata"`
}

func HandleExecuteRequest(receipt MsgReceipt) {
    reply := NewMsg("execute_reply", receipt.Msg)
    content := make(map[string]interface{})
    reqcontent := receipt.Msg.Content.(map[string]interface{})
    code := reqcontent["code"].(string)
    ExecCounter++
    content["execution_count"] = ExecCounter
    val, err := RunCode(code)
    if err == nil {
        content["status"] = "ok"
        content["payload"] = make([]map[string]interface{}, 0)
        content["user_variables"] = make(map[string]string)
        content["user_expressions"] = make(map[string]string)
        if val != nil {
            var out_content OutputMsg
            out := NewMsg("pyout", receipt.Msg)
            out_content.Execcount = ExecCounter
            out_content.Data = make(map[string]string)
            out_content.Data["text/plain"] = fmt.Sprint(val)
            out.Content = out_content
            receipt.SendResponse(receipt.Sockets.IOPub_socket, out)
        }
    } else {
        content["status"] = "error"
        content["ename"] = "ERROR"
        content["evalue"] = err.Error()
        content["traceback"] = []string{err.Error()}
    }
    reply.Content = content
    receipt.SendResponse(receipt.Sockets.Shell_socket, reply)
    idle := NewMsg("status", receipt.Msg)
    idle.Content = KernelStatus{"idle"}
    receipt.SendResponse(receipt.Sockets.IOPub_socket, idle)
}


func RunKernel(connection_file string) {
    World = eval.NewWorld()
    fset = token.NewFileSet()
    var conn_info ConnectionInfo
    bs, err := ioutil.ReadFile(connection_file)
    if err != nil {
        fmt.Println(err)
        return
    }
    err = json.Unmarshal(bs, &conn_info)
    if err != nil {
        fmt.Println("error:", err)
        return
    }
    fmt.Printf("%+v\n", conn_info)
    sockets := PrepareSockets(conn_info)

    pi := zmq.PollItems{
        zmq.PollItem{Socket: sockets.Shell_socket, Events: zmq.POLLIN},
        zmq.PollItem{Socket: sockets.Stdin_socket, Events: zmq.POLLIN},
    }
    var msgparts [][]byte
    for {
        _, err = zmq.Poll(pi, -1)
        if err != nil {
            fmt.Println(err)
            return
        }
        switch {
        case pi[0].REvents&zmq.POLLIN != 0:
            msgparts, _ = pi[0].Socket.RecvMultipart(0)
            msg, ids, err := WireMsgToComposedMsg(msgparts, sockets.Key)
            if err != nil {
                fmt.Println(err)
                return
            }
            HandleShellMsg(MsgReceipt{msg, ids, sockets})
        case pi[1].REvents&zmq.POLLIN != 0:
            pi[1].Socket.RecvMultipart(0)
        }
    }
}