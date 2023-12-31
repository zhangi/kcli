package kafka

import (
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"hash"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	"github.com/xdg/scram"
)

// Decoder is the interface that is required of plugins
type Decoder interface {
	Decode(topic string, data []byte) ([]byte, error)
}

// plainDecoder is the default Decoder
type plainDecoder struct{}

func (p plainDecoder) Decode(topic string, data []byte) ([]byte, error) { return data, nil }

// Client fetches from kafka
type Client struct {
	addrs       []string
	sarama      sarama.Client
	decoder     Decoder
	concurrency int
}

// Partition holds information about a kafka partition
type Partition struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Offset    int64  `json:"offset"`
	Filter    string `json:"filter"`
}

// String turns a partition into a string
func (p *Partition) String() string {
	d, _ := json.Marshal(p)
	return string(d)
}

// Message holds information about a single kafka message
type Message struct {
	Partition Partition `json:"partition"`
	Value     []byte    `json:"msg"`
	Offset    int64     `json:"offset"`
}

// Opt is a func that sets an  attribute on Client
type Opt func(*Client)

// New returns a kafka Client.
func New(addrs []string, opts ...Opt) (*Client, error) {
	cfg, err := getConfig()
	if err != nil {
		return nil, err
	}

	s, err := sarama.NewClient(addrs, cfg)
	if err != nil {
		return nil, err
	}

	cli := &Client{
		sarama:      s,
		addrs:       addrs,
		decoder:     &plainDecoder{},
		concurrency: 20,
	}

	for _, opt := range opts {
		opt(cli)
	}

	return cli, nil
}

// Concurrency is used to set the size of the search worker pool
func Concurrency(j int) func(*Client) {
	return func(c *Client) {
		c.concurrency = j
	}
}

// WithDecoder is used to insert a Decoder plugin
func WithDecoder(d Decoder) func(*Client) {
	return func(c *Client) {
		c.decoder = d
	}
}

var SHA256 scram.HashGeneratorFcn = func() hash.Hash { return sha256.New() }
var SHA512 scram.HashGeneratorFcn = func() hash.Hash { return sha512.New() }

type XDGSCRAMClient struct {
	*scram.Client
	*scram.ClientConversation
	scram.HashGeneratorFcn
}

func (x *XDGSCRAMClient) Begin(userName, password, authzID string) (err error) {
	x.Client, err = x.HashGeneratorFcn.NewClient(userName, password, authzID)
	if err != nil {
		return err
	}
	x.ClientConversation = x.Client.NewConversation()
	return nil
}

func (x *XDGSCRAMClient) Step(challenge string) (response string, err error) {
	response, err = x.ClientConversation.Step(challenge)
	return
}

func (x *XDGSCRAMClient) Done() bool {
	return x.ClientConversation.Done()
}

func getConfig() (*sarama.Config, error) {
	cfg := sarama.NewConfig()

	cfg.Net.SASL.User = os.Getenv("KCLI_USERNAME")
	if cfg.Net.SASL.User != "" {
		cfg.Net.SASL.Enable = true
		cfg.Net.SASL.Password = os.Getenv("KCLI_PASSWORD")
		cfg.Net.SASL.Handshake = true
		cfg.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: SHA512} }
		cfg.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
	}

	tlsCfg, err := getTLSConfig()
	if err != nil || tlsCfg == nil {
		return cfg, err
	}

	cfg.Net.TLS.Enable = true
	cfg.Net.TLS.Config = tlsCfg
	return cfg, nil
}

func getTLSConfig() (*tls.Config, error) {
	certFile := os.Getenv("KCLI_CERT_FILE")
	keyFile := os.Getenv("KCLI_KEY_FILE")
	caCertFile := os.Getenv("KCLI_CA_CERT_FILE")
	if certFile == "" || keyFile == "" || caCertFile == "" {
		return nil, nil
	}

	cfg := tls.Config{}

	// Load client cert
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return &cfg, err
	}
	cfg.Certificates = []tls.Certificate{cert}

	// Load CA cert
	caCert, err := ioutil.ReadFile(caCertFile)
	if err != nil {
		return &cfg, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	cfg.RootCAs = caCertPool

	cfg.BuildNameToCertificate()
	return &cfg, nil
}

// GetTopics gets topics (duh)
func (c *Client) GetTopics() ([]string, error) {
	return c.sarama.Topics()
}

// GetTopic gets a single kafka topic
func (c *Client) GetTopic(topic string) ([]Partition, error) {
	partitions, err := c.sarama.Partitions(topic)
	if err != nil {
		return nil, err
	}

	out := make([]Partition, len(partitions))

	for i, p := range partitions {
		n, err := c.sarama.GetOffset(topic, p, sarama.OffsetNewest)
		if err != nil {
			return nil, err
		}

		o, err := c.sarama.GetOffset(topic, p, sarama.OffsetOldest)
		if err != nil {
			return nil, err
		}
		out[i] = Partition{
			Topic:     topic,
			Partition: p,
			Start:     o,
			End:       n,
			Offset:    o,
		}
	}
	return out, nil
}

// GetPartition fetches a kafka partition.  It includes a callback func
// so that the caller can tell it when to stop consuming.
func (c *Client) GetPartition(part Partition, end int, f func([]byte) bool) ([]Message, error) {
	consumer, err := sarama.NewConsumer(c.addrs, nil)
	if err != nil {
		return nil, err
	}

	pc, err := consumer.ConsumePartition(part.Topic, part.Partition, part.Offset)
	if err != nil {
		return nil, err
	}

	defer func() {
		consumer.Close()
		pc.Close()
	}()

	var out []Message

	var msg *sarama.ConsumerMessage
	var i int
	var last bool
	for i < end && !last {
		select {
		case msg = <-pc.Messages():
			if f(msg.Value) {
				val, err := c.decoder.Decode(part.Topic, msg.Value)
				if err != nil {
					return nil, err
				}

				out = append(out, Message{
					Value:  val,
					Offset: msg.Offset,
					Partition: Partition{
						Offset:    msg.Offset,
						Partition: msg.Partition,
						Topic:     msg.Topic,
						End:       part.End,
					},
				})
				i++
			}
			last = msg.Offset == part.End-1
		case <-time.After(time.Second):
			break
		}
	}

	return out, nil
}

// Close disconnects from kafka
func (c *Client) Close() {
	c.sarama.Close()
}

type searchResult struct {
	partition Partition
	offset    int64
	error     error
}

// SearchTopic allows the caller to search across all partitions in a topic.
func (c *Client) SearchTopic(partitions []Partition, s string, firstResult bool, cb func(int64, int64)) ([]Partition, error) {
	ch := make(chan searchResult)
	in := make(chan Partition)
	n := int64(len(partitions))
	var stop bool
	f := func() bool {
		return stop
	}

	for i := 0; i < c.concurrency; i++ {
		go func(in chan Partition, out chan searchResult) {
			for partition := range in {
				i, err := c.search(partition, s, f, func(_, _ int64) {})
				ch <- searchResult{partition: partition, offset: i, error: err}
			}
		}(in, ch)
	}

	go func() {
		for _, p := range partitions {
			in <- p
		}
	}()

	var results []Partition

	nResults := len(partitions)
	if firstResult {
		nResults = 1
	}

	var i int64
	for i = 0; i < int64(len(partitions)); i++ {
		r := <-ch
		cb(i, n)
		if r.error != nil {
			return nil, r.error
		}
		if r.offset > -1 {
			r.partition.Offset = r.offset
			results = append(results, r.partition)
		}
		if len(results) == nResults {
			stop = true
			break
		}
	}

	close(in)

	sort.Slice(results, func(i, j int) bool {
		return results[j].Partition >= results[i].Partition
	})

	return results, nil
}

func (c *Client) search(info Partition, s string, stop func() bool, cb func(int64, int64)) (int64, error) {
	n := int64(-1)
	var i int64
	err := c.consume(info, info.End, func(d []byte) bool {
		cb(i, info.End)
		if strings.Contains(string(d), s) {
			n = i + info.Offset
			return true
		}
		i++
		return stop()
	})

	return n, err
}

// Search is for searching for a string in a single kafka partition.
// It stops at the first match.
func (c *Client) Search(info Partition, s string, cb func(i, j int64)) (int64, error) {
	return c.search(info, s, func() bool { return false }, cb)
}

// Fetch gets all messages in a partition up intil the 'end' offset.
func (c *Client) Fetch(info Partition, end int64, cb func(string)) error {
	return c.consume(info, end, func(msg []byte) bool {
		val, err := c.decoder.Decode(info.Topic, msg)
		if err != nil {
			return true
		}
		cb(string(val))
		return false
	})
}

func (c *Client) consume(info Partition, end int64, cb func([]byte) bool) error {
	consumer, err := sarama.NewConsumer(c.addrs, nil)
	if err != nil {
		return err
	}

	pc, err := consumer.ConsumePartition(info.Topic, info.Partition, info.Offset)
	if err != nil {
		return err
	}

	defer func() {
		consumer.Close()
		pc.Close()
	}()

	l := info.End - info.Offset
	if l < end {
		end = l
	}

	for i := int64(0); i < end; i++ {
		select {
		case msg := <-pc.Messages():
			if stop := cb(msg.Value); stop {
				return nil
			}
		case <-time.After(time.Second):
			break
		}
	}

	return nil
}
