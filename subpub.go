package nrelay

import(
  "log"
  "strings"

  "github.com/nats-io/go-nats"
)

type MsgQueue chan *nats.Msg
type Subpub struct {
  srcConn    *nats.Conn
  dstConn    *nats.Conn
  msgChan    MsgQueue
  logger     *log.Logger
  sub        *nats.Subscription
  connType   string
  topic      string
  running    bool
}
func NewSubpub(connType string, src, dst *nats.Conn, logger *log.Logger) *Subpub {
  subpub := new(Subpub)
  subpub.srcConn  = src
  subpub.dstConn  = dst
  subpub.msgChan  = make(MsgQueue, 0)
  subpub.connType = connType
  subpub.logger   = logger
  return subpub
}
func (s *Subpub) Subscribe(topic, group string) error {
  sub, err := s.srcConn.ChanQueueSubscribe(topic, group, s.msgChan)
  if err != nil {
    s.logger.Printf("error: topic(%s) subscribe failure: %s", topic, group)
    return err
  }
  s.srcConn.Flush()
  s.sub = sub
  s.topic = topic
  s.running = true
  go s.exchangeLoop()
  return nil
}
func (s *Subpub) exchangeLoop() {
  for s.running {
    select {
    case msg, ok := <-s.msgChan:
      if ok != true {
        return
      }

      // ignore reply
      s.dstConn.Publish(msg.Subject, msg.Data)
    }
  }
}
func (s *Subpub) Close() error {
  if err := s.sub.Unsubscribe(); err != nil {
    return err
  }
  s.running = false
  close(s.msgChan)

  return nil
}
func (s *Subpub) String() string {
  return strings.Join([]string{"subpub", s.connType, s.topic}, "/")
}
