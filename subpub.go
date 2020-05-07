package nrelay

import(
  "log"
  "sync"
  "sync/atomic"
  "time"
  "strings"

  "github.com/nats-io/nats.go"
  "github.com/lafikl/consistent"
  "github.com/rs/xid"
)

var(
  flushTO  = 5 * time.Millisecond
)

type ConnFactory func(id int) (*nats.Conn, error)

type Pub struct {
  subject   string
  data      []byte
}
type PubQueue    chan *Pub
type DonePubLoop chan struct{}
type PubWorker struct {
  id     string
  conn   *nats.Conn
  logger *log.Logger
  queue  PubQueue
  done   DonePubLoop
  ready  int32
}
func newPubWorker(id string, dst *nats.Conn, logger *log.Logger) *PubWorker {
  w       := new(PubWorker)
  w.id     = id
  w.conn   = dst
  w.logger = logger
  w.queue  = make(PubQueue, 0)
  w.done   = make(DonePubLoop)
  w.ready  = int32(1)
  return w
}
func (w *PubWorker) isReady() bool {
  if atomic.LoadInt32(&w.ready) == 0 {
    return false
  }
  return true
}
func (w *PubWorker) setBusy() {
  atomic.StoreInt32(&w.ready, int32(0))
}
func (w *PubWorker) setReady() {
  atomic.StoreInt32(&w.ready, int32(1))
}
func (w *PubWorker) publish(subject string, data []byte) {
  w.queue <-&Pub{subject, data}
}
func (w *PubWorker) stop() {
  w.done <-struct{}{}
  close(w.done)

  if err := w.conn.Drain(); err != nil {
    w.logger.Printf("warn: close drain error: %s", err.Error())
  }
  w.conn.Close()
}
func (w *PubWorker) start() {
  go w.runPubLoop()
}
func (w *PubWorker) pub(queue []*Pub, done func()) {
  defer done()

  for _, d := range queue {
    w.conn.Publish(d.subject, d.data)
  }
  w.conn.FlushTimeout(flushTO)
}
func (w *PubWorker) runPubLoop() {
  defer w.logger.Printf("debug: %s publoop done", w.id)

  buffer  := make([]*Pub, 0)
  checker := make(chan struct{}, 0)
  check   := func(c chan struct{}) func() {
    return func(){
      c <-struct{}{}
    }
  }(checker)

  for {
    select {
    case <-w.done:
      return

    case d := <-w.queue:
      buffer = append(buffer, d)
      go check()

    case <-checker:
      if len(buffer) < 1 {
        continue
      }

      if w.isReady() {
        w.setBusy()

        queue := make([]*Pub, len(buffer))
        copy(queue, buffer)
        buffer = buffer[len(buffer):] // clear

        go w.pub(queue, func(){
          w.setReady()
          go check()
        })
      }
    }
  }
}

type Subpub struct {
  mutex      *sync.Mutex
  srcConn    *nats.Conn
  connType   string
  logger     *log.Logger
  factory    ConnFactory
  sharding   *consistent.Consistent
  dstMap     *sync.Map
  sids       []string
  fallback   *nats.Conn

  dstConns   []*nats.Conn
  subs       []*nats.Subscription
  topic      string
}
func NewSubpub(connType string, src *nats.Conn, logger *log.Logger, factory ConnFactory) *Subpub {
  s         := new(Subpub)
  s.mutex    = new(sync.Mutex)
  s.srcConn  = src
  s.connType = connType
  s.logger   = logger
  s.factory  = factory
  s.sharding = consistent.New()
  s.dstMap   = new(sync.Map)
  return s
}
func (s *Subpub) makeDispatcher(fallback *nats.Conn, prefixSize int, useLoadBalance bool) nats.MsgHandler {
  if prefixSize < 1 {
    prefixSize = 0
  }
  return func(msg *nats.Msg) {
    subj := msg.Subject
    if 0 < prefixSize && prefixSize < len(subj) {
      subj = subj[0 : prefixSize]
    }
    sid, err := s.sharding.GetLeast(subj)
    if err != nil {
      fallback.Publish(msg.Subject, msg.Data)
      fallback.FlushTimeout(flushTO)
      return
    }
    val, ok := s.dstMap.Load(sid)
    if ok != true {
      fallback.Publish(msg.Subject, msg.Data)
      fallback.FlushTimeout(flushTO)
      return
    }

    if useLoadBalance {
      s.sharding.Inc(sid)
      defer s.sharding.Done(sid)
    }

    worker := val.(*PubWorker)
    worker.publish(msg.Subject, msg.Data)
  }
}
func (s *Subpub) Subscribe(topic, group string, numWorker, numShard int, prefixSize int, useLoadBalance bool) error {
  s.mutex.Lock()
  defer s.mutex.Unlock()

  if numShard < 1 {
    numShard = 1
  }

  sids := make([]string, 0, numShard)
  for i := 0; i < numShard; i += 1 {
    dst, err := s.factory(i + 1)
    if err != nil {
      s.logger.Printf("error: relay nats(%d) connection failure: %s", i, err.Error())
      return err
    }

    sid    := xid.New().String()
    worker := newPubWorker(sid, dst, s.logger)
    worker.start()

    s.sharding.Add(sid)
    s.dstMap.Store(sid, worker)
    sids = append(sids, sid)
  }
  s.sids = sids

  fallbackConn, err := s.factory(0)
  if err != nil {
    s.logger.Printf("error: relay fallback conn failure: %s", err.Error())
    return err
  }
  s.fallback = fallbackConn

  subs := make([]*nats.Subscription, numWorker)
  for i := 0; i < numWorker; i += 1 {
    d := s.makeDispatcher(fallbackConn, prefixSize, useLoadBalance)
    sub, err := s.srcConn.QueueSubscribe(topic, group, d)
    if err != nil {
      s.logger.Printf("error: topic(%s) subscribe failure: %s", topic, group)
      return err
    }
    subs[i] = sub
  }
  s.srcConn.Flush()

  s.subs    = subs
  s.topic   = topic
  return nil
}
func (s *Subpub) Close() error {
  s.mutex.Lock()
  defer s.mutex.Unlock()

  for _, sub := range s.subs {
    if err := sub.Unsubscribe(); err != nil {
      return err
    }
  }
  s.srcConn.Drain()

  if 0 < len(s.sids) {
    for _, sid := range s.sids {
      if val, ok := s.dstMap.Load(sid); ok {
        worker := val.(*PubWorker)
        worker.stop()
        s.dstMap.Delete(sid)
      }
    }
  }
  if s.fallback != nil {
    if err := s.fallback.Drain(); err != nil {
      s.logger.Printf("warn: close fallback drain error: %s", err.Error())
    }
    s.fallback.Close()
  }

  return nil
}
func (s *Subpub) String() string {
  return strings.Join([]string{"subpub", s.connType, s.topic}, "/")
}
