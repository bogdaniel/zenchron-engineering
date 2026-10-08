package conformance

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// Reply is one scripted transport outcome.
type Reply struct {
	// Status and Body form a raw HTTP reply (Status 0 means 200).
	Status int
	Body   []byte
	// Err fails the round trip at the transport.
	Err error
	// Block holds the round trip until the request context ends.
	Block bool
}

// Transport is a fake wire.Doer that replays replies in order and records
// every request. It never touches the network.
type Transport struct {
	mu      sync.Mutex
	replies []Reply
	sent    []Sent
	// Entered receives one value as each round trip starts.
	Entered chan struct{}
}

// Sent is one captured request.
type Sent struct {
	URL    string
	Header http.Header
	Body   []byte
}

// NewTransport returns a transport that answers with replies in order.
func NewTransport(replies ...Reply) *Transport {
	return &Transport{replies: replies, Entered: make(chan struct{}, len(replies)+1)}
}

// Do implements wire.Doer.
func (t *Transport) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.sent = append(t.sent, Sent{URL: req.URL.String(), Header: req.Header.Clone(), Body: body})
	var reply Reply
	if len(t.replies) > 0 {
		reply, t.replies = t.replies[0], t.replies[1:]
	} else {
		reply = Reply{Status: http.StatusInternalServerError, Body: []byte(`{"error":{"message":"transport script exhausted"}}`)}
	}
	t.mu.Unlock()
	t.Entered <- struct{}{}
	if reply.Block {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if reply.Err != nil {
		return nil, reply.Err
	}
	status := reply.Status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(reply.Body)), Header: http.Header{}}, nil
}

// Sent returns a copy of the captured requests.
func (t *Transport) Sent() []Sent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Sent(nil), t.sent...)
}
