//go:generate mockgen -destination=../../mocks/services/stats/mock_stats.go -package mock_stats github.com/rudderlabs/rudder-server/services/stats Stats,RudderStats
package stats

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/rudderlabs/rudder-server/config"
	"github.com/rudderlabs/rudder-server/rruntime"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/types/deployment"
	"gopkg.in/alexcesaro/statsd.v2"
)

const (
	CountType     = "count"
	TimerType     = "timer"
	GaugeType     = "gauge"
	HistogramType = "histogram"
)

var (
	statsEnabled            bool
	statsTagsFormat         string
	statsdServerURL         string
	instanceID              string
	enabled                 bool
	statsCollectionInterval int64
	enableCPUStats          bool
	enableMemStats          bool
	enableGCStats           bool
	statsSamplingRate       float32

	pkgLogger logger.LoggerI

	conn   statsd.Option
	client *statsd.Client
	rc     runtimeStatsCollector
	mc     metricStatsCollector

	taggedClientsMapLock    sync.RWMutex
	taggedClientsMap        = make(map[string]*statsd.Client)
	connEstablished         bool
	taggedClientPendingKeys [][]string
	taggedClientPendingTags []string
)

// DefaultStats is a common implementation of StatsD stats managements
var DefaultStats Stats

func Init() {
	config.RegisterBoolConfigVariable(true, &statsEnabled, false, "enableStats")
	config.RegisterStringConfigVariable("influxdb", &statsTagsFormat, false, "statsTagsFormat")
	statsdServerURL = config.GetEnv("STATSD_SERVER_URL", "localhost:8125")
	instanceID = config.GetEnv("INSTANCE_ID", "")
	config.RegisterBoolConfigVariable(true, &enabled, false, "RuntimeStats.enabled")
	config.RegisterInt64ConfigVariable(10, &statsCollectionInterval, false, 1, "RuntimeStats.statsCollectionInterval")
	config.RegisterBoolConfigVariable(true, &enableCPUStats, false, "RuntimeStats.enableCPUStats")
	config.RegisterBoolConfigVariable(true, &enableMemStats, false, "RuntimeStats.enabledMemStats")
	config.RegisterBoolConfigVariable(true, &enableGCStats, false, "RuntimeStats.enableGCStats")
	statsSamplingRate = float32(config.GetFloat64("statsSamplingRate", 1))

	pkgLogger = logger.NewLogger().Child("stats")
}

type Tags map[string]string

// Strings returns all key value pairs as an ordered list of strings, sorted by increasing key order
func (t Tags) Strings() []string {
	if len(t) == 0 {
		return nil
	}
	res := make([]string, 0, len(t)*2)
	// sorted by tag name (!important for consistent map iteration order)
	tagNames := make([]string, 0, len(t))
	for n := range t {
		tagNames = append(tagNames, n)
	}
	sort.Strings(tagNames)
	for _, tagName := range tagNames {
		tagVal := t[tagName]
		res = append(res, strings.ReplaceAll(tagName, ":", "-"), strings.ReplaceAll(tagVal, ":", "-"))
	}
	return res
}

// String returns all key value pairs as a single string, separated by commas, sorted by increasing key order
func (t Tags) String() string {
	return strings.Join(t.Strings(), ",")
}

// Stats manages provisioning of RudderStats
type Stats interface {
	NewStat(Name, StatType string) (rStats RudderStats)
	NewTaggedStat(Name, StatType string, tags Tags) RudderStats
	NewSampledTaggedStat(Name, StatType string, tags Tags) RudderStats
}

// HandleT is the default implementation of Stats
type HandleT struct{}

// RudderStats provides functions to interact with StatsD stats
type RudderStats interface {
	Count(n int)
	Increment()

	Gauge(value interface{})

	Start()
	End()
	Observe(value float64)
	SendTiming(duration time.Duration)
	Since(start time.Time)
}

// RudderStatsT is the default implementation of a StatsD stat
type RudderStatsT struct {
	Name        string
	StatType    string
	Timing      statsd.Timing
	DestID      string
	Client      *statsd.Client
	dontProcess bool
}

func getNewStatsdClientWithExpoBackoff(opts ...statsd.Option) (*statsd.Client, error) {
	bo := backoff.NewExponentialBackOff()
	bo.MaxInterval = time.Minute
	bo.MaxElapsedTime = 0
	var err error
	var c *statsd.Client
	newClient := func() error {
		c, err = statsd.New(opts...)
		if err != nil {
			pkgLogger.Errorf("error while setting statsd client: %v", err)
		}
		return err
	}

	if err = backoff.Retry(newClient, bo); err != nil {
		if bo.NextBackOff() == backoff.Stop {
			return nil, err
		}
	}
	return c, nil
}

// Setup creates a new statsd client
func Setup() {
	DefaultStats = &HandleT{}

	if !statsEnabled {
		return
	}
	conn = statsd.Address(statsdServerURL)
	// since, we don't want setup to be a blocking call, creating a separate `go routine`` for retry to get statsd client.
	var err error
	// NOTE: this is to get atleast a dummy client, even if there is a failure. So, that nil pointer error is not received when client is called.
	client, err = statsd.New(conn, statsd.TagsFormat(getTagsFormat()), defaultTags())
	if err == nil {
		taggedClientsMapLock.Lock()
		connEstablished = true
		taggedClientsMapLock.Unlock()
	}
	rruntime.Go(func() {
		if err != nil {
			connEstablished = false
			c, err := getNewStatsdClientWithExpoBackoff(conn, statsd.TagsFormat(getTagsFormat()), defaultTags())
			if err != nil {
				statsEnabled = false
				pkgLogger.Errorf("error while creating new statsd client: %v", err)
			} else {
				client = c
				taggedClientsMapLock.Lock()
				for i, tagValue := range taggedClientPendingKeys {
					taggedClient := client.Clone(conn, statsd.TagsFormat(getTagsFormat()), defaultTags(), statsd.Tags(tagValue...), statsd.SampleRate(statsSamplingRate))
					taggedClientsMap[taggedClientPendingTags[i]] = taggedClient
				}

				pkgLogger.Info("statsd client setup succeeded.")
				connEstablished = true

				taggedClientPendingKeys = nil
				taggedClientPendingTags = nil
				taggedClientsMapLock.Unlock()
			}
		}

		collectPeriodicStats(client)
	})
}

// NewStat creates a new RudderStats with provided Name and Type
func (s *HandleT) NewStat(Name, StatType string) (rStats RudderStats) {
	return &RudderStatsT{
		Name:     Name,
		StatType: StatType,
		Client:   client,
	}
}

func (s *HandleT) NewTaggedStat(Name, StatType string, tags Tags) (rStats RudderStats) {
	return newTaggedStat(Name, StatType, tags, 1)
}

func (s *HandleT) NewSampledTaggedStat(Name, StatType string, tags Tags) (rStats RudderStats) {
	return newTaggedStat(Name, StatType, tags, statsSamplingRate)
}

func CleanupTagsBasedOnDeploymentType(tags Tags, key string) Tags {
	deploymentType, err := deployment.GetFromEnv()
	if err != nil {
		pkgLogger.Errorf("error while getting deployment type: %w", err)
		return tags
	}

	if deploymentType == deployment.MultiTenantType {
		isNamespaced := config.IsEnvSet("WORKSPACE_NAMESPACE")
		if !isNamespaced || (isNamespaced && strings.Contains(config.GetEnv("WORKSPACE_NAMESPACE", ""), "free")) {
			delete(tags, key)
		}
	}

	return tags
}

func newTaggedStat(Name, StatType string, tags Tags, samplingRate float32) (rStats RudderStats) {
	// If stats is not enabled, returning a dummy struct
	if !statsEnabled {
		return &RudderStatsT{
			Name:        Name,
			StatType:    StatType,
			Client:      nil,
			dontProcess: true,
		}
	}

	// Clean up tags based on deployment type. No need to send workspace id tag for free tier customers.
	tags = CleanupTagsBasedOnDeploymentType(tags, "workspaceId")

	if tags == nil {
		tags = make(Tags)
	}
	// key comprises of the measurement name plus all tag-value pairs
	taggedClientKey := StatType + tags.String()

	taggedClientsMapLock.RLock()
	taggedClient, found := taggedClientsMap[taggedClientKey]
	taggedClientsMapLock.RUnlock()

	if !found {
		taggedClientsMapLock.Lock()
		if taggedClient, found = taggedClientsMap[taggedClientKey]; !found { // double check for race
			tagVals := tags.Strings()
			if !connEstablished {
				taggedClientPendingTags = append(taggedClientPendingTags, taggedClientKey)
				taggedClientPendingKeys = append(taggedClientPendingKeys, tagVals)
			}
			taggedClient = client.Clone(conn, statsd.TagsFormat(getTagsFormat()), defaultTags(), statsd.Tags(tagVals...), statsd.SampleRate(samplingRate))
			taggedClientsMap[taggedClientKey] = taggedClient
		}
		taggedClientsMapLock.Unlock()
	}

	return &RudderStatsT{
		Name:        Name,
		StatType:    StatType,
		Client:      taggedClient,
		dontProcess: false,
	}
}

func NewTaggedStat(Name, StatType string, tags Tags) (rStats RudderStats) {
	return DefaultStats.NewTaggedStat(Name, StatType, tags)
}

func (rStats *RudderStatsT) Count(n int) {
	if !statsEnabled || rStats.dontProcess {
		return
	}
	if rStats.StatType != CountType {
		panic(fmt.Errorf("rStats.StatType:%s is not count", rStats.StatType))
	}
	rStats.Client.Count(rStats.Name, n)
}

// Increment increases the stat by 1. Is the Equivalent of Count(1). Only applies to CountType stats
func (rStats *RudderStatsT) Increment() {
	if !statsEnabled || rStats.dontProcess {
		return
	}
	if rStats.StatType != CountType {
		panic(fmt.Errorf("rStats.StatType:%s is not count", rStats.StatType))
	}
	rStats.Client.Increment(rStats.Name)
}

// Gauge records an absolute value for this stat. Only applies to GaugeType stats
func (rStats *RudderStatsT) Gauge(value interface{}) {
	if !statsEnabled || rStats.dontProcess {
		return
	}
	if rStats.StatType != GaugeType {
		panic(fmt.Errorf("rStats.StatType:%s is not gauge", rStats.StatType))
	}
	rStats.Client.Gauge(rStats.Name, value)
}

// Start starts a new timing for this stat. Only applies to TimerType stats

func (rStats *RudderStatsT) Start() {
	if !statsEnabled || rStats.dontProcess {
		return
	}
	if rStats.StatType != TimerType {
		panic(fmt.Errorf("rStats.StatType:%s is not timer", rStats.StatType))
	}
	rStats.Timing = rStats.Client.NewTiming()
}

// End send the time elapsed since the Start()  call of this stat. Only applies to TimerType stats
// Deprecated: Use concurrent safe SendTiming() instead
func (rStats *RudderStatsT) End() {
	if !statsEnabled || rStats.dontProcess {
		return
	}
	if rStats.StatType != TimerType {
		panic(fmt.Errorf("rStats.StatType:%s is not timer", rStats.StatType))
	}
	rStats.Timing.Send(rStats.Name)
}

// Since sends the time elapsed since duration start. Only applies to TimerType stats
func (rStats *RudderStatsT) Since(start time.Time) {
	rStats.SendTiming(time.Since(start))
}

// Timing sends a timing for this stat. Only applies to TimerType stats
func (rStats *RudderStatsT) SendTiming(duration time.Duration) {
	if !statsEnabled || rStats.dontProcess {
		return
	}
	if rStats.StatType != TimerType {
		panic(fmt.Errorf("rStats.StatType:%s is not timer", rStats.StatType))
	}
	rStats.Client.Timing(rStats.Name, int(duration/time.Millisecond))
}

// Timing sends a timing for this stat. Only applies to TimerType stats
func (rStats *RudderStatsT) Observe(value float64) {
	if !statsEnabled || rStats.dontProcess {
		return
	}
	if rStats.StatType != HistogramType {
		panic(fmt.Errorf("rStats.StatType:%s is not histogram", rStats.StatType))
	}
	rStats.Client.Histogram(rStats.Name, value)
}

func collectPeriodicStats(client *statsd.Client) {
	gaugeFunc := func(key string, val uint64) {
		client.Gauge("runtime_"+key, val)
	}
	rc = newRuntimeStatsCollector(gaugeFunc)
	rc.PauseDur = time.Duration(statsCollectionInterval) * time.Second
	rc.EnableCPU = enableCPUStats
	rc.EnableMem = enableMemStats
	rc.EnableGC = enableGCStats

	mc = newMetricStatsCollector()
	if enabled {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			rc.run()
		}()
		go func() {
			defer wg.Done()
			mc.run()
		}()
		wg.Wait()
	}
}

// StopPeriodicStats stops periodic collection of stats.
func StopPeriodicStats() {
	taggedClientsMapLock.RLock()
	defer taggedClientsMapLock.RUnlock()
	if !statsEnabled || !connEstablished {
		return
	}

	close(rc.Done)
	close(mc.done)
}

func getTagsFormat() statsd.TagFormat {
	switch statsTagsFormat {
	case "datadog":
		return statsd.Datadog
	case "influxdb":
		return statsd.InfluxDB
	default:
		return statsd.InfluxDB
	}
}

// returns default Tags to the telegraf request
func defaultTags() statsd.Option {
	if len(config.GetKubeNamespace()) > 0 {
		return statsd.Tags("instanceName", instanceID, "namespace", config.GetKubeNamespace())
	}
	return statsd.Tags("instanceName", instanceID)
}
