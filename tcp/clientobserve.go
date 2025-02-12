package tcp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/plgd-dev/go-coap/v2/message"
	"github.com/plgd-dev/go-coap/v2/message/codes"
	"github.com/plgd-dev/go-coap/v2/net/observation"
	"github.com/plgd-dev/go-coap/v2/tcp/message/pool"
	"go.uber.org/atomic"
)

func NewObservationHandler(obsertionTokenHandler *HandlerContainer, next HandlerFunc) HandlerFunc {
	return func(w *ResponseWriter, r *pool.Message) {
		v, err := obsertionTokenHandler.Get(r.Token())
		if err != nil {
			next(w, r)
			return
		}
		v(w, r)
	}
}

type respObservationMessage struct {
	code         codes.Code
	notSupported bool
}

//Observation represents subscription to resource on the server
type Observation struct {
	token               message.Token
	path                string
	cc                  *ClientConn
	observeFunc         func(req *pool.Message)
	respObservationChan chan respObservationMessage
	waitForResponse     atomic.Bool

	mutex       sync.Mutex
	obsSequence uint32    // guarded by mutex
	lastEvent   time.Time // guarded by mutex
}

func (o *Observation) Canceled() bool {
	_, ok := o.cc.observationRequests.Load(o.token.Hash())
	return !ok
}

func newObservation(token message.Token, path string, cc *ClientConn, observeFunc func(req *pool.Message), respObservationChan chan respObservationMessage) *Observation {
	return &Observation{
		token:               token,
		path:                path,
		obsSequence:         0,
		cc:                  cc,
		waitForResponse:     *atomic.NewBool(true),
		respObservationChan: respObservationChan,
		observeFunc:         observeFunc,
	}
}

func (o *Observation) handler(w *ResponseWriter, r *pool.Message) {
	code := r.Code()
	notSupported := !r.HasOption(message.Observe)
	if o.waitForResponse.CAS(true, false) {
		select {
		case o.respObservationChan <- respObservationMessage{
			code:         code,
			notSupported: notSupported,
		}:
		default:
		}
		o.respObservationChan = nil
	}
	if o.wantBeNotified(r) {
		o.observeFunc(r)
	}
}

func (o *Observation) cleanUp() bool {
	o.cc.observationTokenHandler.Pop(o.token)
	_, ok := o.cc.observationRequests.PullOut(o.token.Hash())
	return ok
}

// Cancel remove observation from server. For recreate observation use Observe.
func (o *Observation) Cancel(ctx context.Context) error {
	if !o.cleanUp() {
		// observation was already cleanup
		return nil
	}
	req, err := NewGetRequest(ctx, o.cc.session.messagePool, o.path)
	if err != nil {
		return fmt.Errorf("cannot cancel observation request: %w", err)
	}
	defer o.cc.session.messagePool.ReleaseMessage(req)
	req.SetObserve(1)
	req.SetToken(o.token)
	resp, err := o.cc.Do(req)
	if err != nil {
		return err
	}
	defer o.cc.session.messagePool.ReleaseMessage(resp)
	if resp.Code() != codes.Content {
		return fmt.Errorf("unexpected return code(%v)", resp.Code())
	}
	return nil
}

func (o *Observation) wantBeNotified(r *pool.Message) bool {
	obsSequence, err := r.Observe()
	if err != nil {
		return true
	}
	now := time.Now()

	o.mutex.Lock()
	defer o.mutex.Unlock()
	if observation.ValidSequenceNumber(o.obsSequence, obsSequence, o.lastEvent, now) {
		o.obsSequence = obsSequence
		o.lastEvent = now
		return true
	}

	return false
}

// Observe subscribes for every change of resource on path.
func (cc *ClientConn) Observe(ctx context.Context, path string, observeFunc func(req *pool.Message), opts ...message.Option) (*Observation, error) {
	req, err := NewGetRequest(ctx, cc.session.messagePool, path, opts...)
	if err != nil {
		return nil, fmt.Errorf("cannot create observe request: %w", err)
	}
	defer cc.session.messagePool.ReleaseMessage(req)
	token := req.Token()
	req.SetObserve(0)

	respObservationChan := make(chan respObservationMessage, 1)
	o := newObservation(token, path, cc, observeFunc, respObservationChan)

	options, err := req.Options().Clone()
	if err != nil {
		return nil, fmt.Errorf("cannot clone options: %w", err)
	}

	obs := message.Message{
		Context: req.Context(),
		Token:   req.Token(),
		Code:    req.Code(),
		Options: options,
	}
	cc.observationRequests.Store(token.Hash(), obs)
	err = o.cc.observationTokenHandler.Insert(token.Hash(), o.handler)
	defer func(err *error) {
		if *err != nil {
			o.cleanUp()
		}
	}(&err)
	if err != nil {
		return nil, err
	}

	err = cc.WriteMessage(req)
	if err != nil {
		return nil, err
	}
	select {
	case <-req.Context().Done():
		err = req.Context().Err()
		return nil, err
	case <-cc.Context().Done():
		err = fmt.Errorf("connection was closed: %w", cc.Context().Err())
		return nil, err
	case respObservationMessage := <-respObservationChan:
		if respObservationMessage.code != codes.Content {
			err = fmt.Errorf("unexpected return code(%v)", respObservationMessage.code)
			return nil, err
		}
		if respObservationMessage.notSupported {
			o.cleanUp()
		}
		return o, nil
	}
}
