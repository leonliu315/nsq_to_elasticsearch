// heavily modified version of https://github.com/bitly/nsq/blob/master/apps/nsq_to_http/nsq_to_http.go
// Modified by lxfontes to index elasticsearch items

package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bitly/go-nsq"
	"github.com/bitly/nsq/util"
	"github.com/bitly/nsq/util/timermetrics"
	"github.com/jehiah/go-strftime"
	"github.com/olivere/elastic"
)

var (
	showVersion = flag.Bool("version", false, "print version string")

	topic       = flag.String("topic", ".*", "nsq topic pattern")
	channel     = flag.String("channel", "nsq_to_elasticsearch", "nsq channel")
	maxInFlight = flag.Int("max-in-flight", 200, "max number of messages to allow in flight")

	numPublishers   = flag.Int("n", 10, "number of concurrent publishers")
	httpTimeout     = flag.Duration("http-timeout", 20*time.Second, "timeout for HTTP connect/read/write (each)")
	refreshInterval = flag.Duration("refresh-interval", 1*time.Minute, "topic discovery refresh interval")
	statusEvery     = flag.Int("status-every", 250, "the # of requests between logging status (per handler), 0 disables")

	indexName = flag.String("index-name", "logstash-%Y.%m.%d", "elasticsearch index name (strftime format)")
	indexType = flag.String("index-type", "logstash", "elasticsearch index mapping")

	consumerOpts     = util.StringArray{}
	elasticAddrs     = util.StringArray{}
	nsqdTCPAddrs     = util.StringArray{}
	lookupdHTTPAddrs = util.StringArray{}
)

func init() {
	flag.Var(&consumerOpts, "consumer-opt", "option to passthrough to nsq.Consumer (may be given multiple times, http://godoc.org/github.com/bitly/go-nsq#Config)")

	flag.Var(&elasticAddrs, "elasticsearch", "Elasticsearch HTTP address (may be given multiple times)")
	flag.Var(&lookupdHTTPAddrs, "lookupd-http-address", "lookupd HTTP address (may be given multiple times)")
}

func timeoutClient() *http.Client {
	TimeoutDialer := func(timeout time.Duration) func(net, addr string) (c net.Conn, err error) {
		return func(netw, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout(netw, addr, timeout)
			if err != nil {
				return nil, err
			}
			conn.SetDeadline(time.Now().Add(timeout))
			return conn, nil
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			Dial: TimeoutDialer(*httpTimeout),
		},
	}
}

type ElasticFactory struct {
	idxName        string
	idxType        string
	metricsTimeout int
	elasticAddrs   []string
	wg             sync.WaitGroup
	mtx            sync.Mutex
	consumers      []*nsq.Consumer
}

func NewElasticFactory() (*ElasticFactory, error) {
	return &ElasticFactory{}, nil
}

func (f *ElasticFactory) Signal(sig os.Signal) {
	f.Stop()
}

func (f *ElasticFactory) startConsumer(consumer *nsq.Consumer) {
	f.wg.Add(1)
	defer f.wg.Done()
	<-consumer.StopChan
}

func (f *ElasticFactory) RegisterTopic(name string) error {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	log.Println("Registering topic ", name)
	publisher, err := NewElasticPublisher(*indexName, *indexType, *statusEvery, []string(elasticAddrs))
	if err != nil {
		log.Fatal(err)
	}

	cfg := nsq.NewConfig()
	cfg.UserAgent = fmt.Sprintf("nsq_to_elasticsearch/%s go-nsq/%s", util.BINARY_VERSION, nsq.VERSION)
	err = util.ParseOpts(cfg, consumerOpts)
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxInFlight = *maxInFlight

	consumer, err := nsq.NewConsumer(name, *channel, cfg)
	if err != nil {
		log.Fatal(err)
	}

	consumer.AddConcurrentHandlers(publisher, *numPublishers)

	err = consumer.ConnectToNSQDs(nsqdTCPAddrs)
	if err != nil {
		log.Fatal(err)
	}

	err = consumer.ConnectToNSQLookupds(lookupdHTTPAddrs)
	if err != nil {
		log.Fatal(err)
	}

	f.consumers = append(f.consumers, consumer)
	go f.startConsumer(consumer)
	return nil
}

func (f *ElasticFactory) Stop() {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	for _, consumer := range f.consumers {
		consumer.Stop()
	}
	f.wg.Wait()
}

type ElasticPublisher struct {
	client  *elastic.Client
	idxName string
	idxType string
	metrics *timermetrics.TimerMetrics
}

func NewElasticPublisher(indexName string, indexType string, metricsTimeout int, addrs []string) (*ElasticPublisher, error) {
	var err error
	p := &ElasticPublisher{
		idxName: indexName,
		idxType: indexType,
	}
	p.metrics = timermetrics.NewTimerMetrics(metricsTimeout, "[metrics]:")
	p.client, err = elastic.NewClient(timeoutClient(), addrs...)
	return p, err
}

func (p *ElasticPublisher) indexName() string {
	tm := time.Now()
	return strftime.Format(p.idxName, tm)
}

func (p *ElasticPublisher) indexType() string {
	return p.idxType
}

func (p *ElasticPublisher) HandleMessage(m *nsq.Message) error {
	startTime := time.Now()
	entry := p.client.Index().Index(p.indexName()).Type(p.indexType()).BodyString(string(m.Body))
	_, err := entry.Do()
	p.metrics.Status(startTime)
	return err
}

func main() {
	var err error

	flag.Parse()

	if *showVersion {
		fmt.Printf("nsq_to_elasticsearch v%s\n", util.BINARY_VERSION)
		return
	}

	if *topic == "" || *channel == "" {
		log.Fatal("--topic and --channel are required")
	}

	if len(lookupdHTTPAddrs) == 0 {
		log.Fatal("missing --lookupd-http-address")
	}

	if len(elasticAddrs) == 0 {
		log.Fatal("missing --elasticsearch addresses")
	}

	termChan := make(chan os.Signal, 1)
	signal.Notify(termChan, syscall.SIGINT, syscall.SIGTERM)

	elasticFactory, err := NewElasticFactory()

	discoveryCfg := TopicDiscovererConfig{
		LookupdAddresses: []string(lookupdHTTPAddrs),
		Pattern:          *topic,
		Refresh:          *refreshInterval,
		Handler:          elasticFactory.RegisterTopic,
	}
	discoverer, err := NewTopicDiscoverer(discoveryCfg)
	if err != nil {
		log.Fatal(err)
	}

	go discoverer.Start()

	for {
		select {
		case sig := <-termChan:
			discoverer.Signal(sig)
			elasticFactory.Signal(sig)
			return
		}
	}
}
