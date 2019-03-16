package main

import (
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

const (
	namespace   = "ipmi"
	targetLocal = ""
)

var (
	ipmiDCMICurrentPowerRegex    = regexp.MustCompile(`^Current Power\s*:\s*(?P<value>[0-9.]*)\s*Watts.*`)
	bmcInfoFirmwareRevisionRegex = regexp.MustCompile(`^Firmware Revision\s*:\s*(?P<value>[0-9.]*).*`)
	bmcInfoManufacturerIDRegex   = regexp.MustCompile(`^Manufacturer ID\s*:\s*(?P<value>.*)`)
)

type collector struct {
	target string
	config *SafeConfig
}

type sensorData struct {
	ID    int64
	Name  string
	Type  string
	State string
	Value float64
	Unit  string
	Event string
}

type rmcpConfig struct {
	host string
	user string
	pass string
}

var (
	sensorStateDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "sensor", "state"),
		"Indicates the severity of the state reported by an IPMI sensor (0=nominal, 1=warning, 2=critical).",
		[]string{"id", "name", "type"},
		nil,
	)

	sensorValueDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "sensor", "value"),
		"Generic data read from an IPMI sensor of unknown type, relying on labels for context.",
		[]string{"id", "name", "type"},
		nil,
	)

	fanSpeedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "fan_speed", "rpm"),
		"Fan speed in rotations per minute.",
		[]string{"id", "name"},
		nil,
	)

	fanSpeedStateDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "fan_speed", "state"),
		"Reported state of a fan speed sensor (0=nominal, 1=warning, 2=critical).",
		[]string{"id", "name"},
		nil,
	)

	temperatureDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "temperature", "celsius"),
		"Temperature reading in degree Celsius.",
		[]string{"id", "name"},
		nil,
	)

	temperatureStateDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "temperature", "state"),
		"Reported state of a temperature sensor (0=nominal, 1=warning, 2=critical).",
		[]string{"id", "name"},
		nil,
	)

	voltageDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "voltage", "volts"),
		"Voltage reading in Volts.",
		[]string{"id", "name"},
		nil,
	)

	voltageStateDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "voltage", "state"),
		"Reported state of a voltage sensor (0=nominal, 1=warning, 2=critical).",
		[]string{"id", "name"},
		nil,
	)

	currentDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "current", "amperes"),
		"Current reading in Amperes.",
		[]string{"id", "name"},
		nil,
	)

	currentStateDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "current", "state"),
		"Reported state of a current sensor (0=nominal, 1=warning, 2=critical).",
		[]string{"id", "name"},
		nil,
	)

	powerDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "power", "watts"),
		"Power reading in Watts.",
		[]string{"id", "name"},
		nil,
	)

	powerStateDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "power", "state"),
		"Reported state of a power sensor (0=nominal, 1=warning, 2=critical).",
		[]string{"id", "name"},
		nil,
	)

	powerConsumption = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "dcmi", "power_consumption_watts"),
		"Current power consumption in Watts.",
		[]string{},
		nil,
	)

	bmcInfo = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "bmc", "info"),
		"Constant metric with value '1' providing details about the BMC.",
		[]string{"firmware_revision", "manufacturer_id"},
		nil,
	)

	upDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "up"),
		"'1' if a scrape of the IPMI device was successful, '0' otherwise.",
		[]string{"collector"},
		nil,
	)

	durationDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "scrape_duration", "seconds"),
		"Returns how long the scrape took to complete in seconds.",
		nil,
		nil,
	)
)

func pipeName() string {
	randBytes := make([]byte, 16)
	rand.Read(randBytes)
	return filepath.Join(os.TempDir(), "ipmi_exporter-"+hex.EncodeToString(randBytes))
}

func freeipmiConfig(driver, user, password string) string {
	return fmt.Sprintf(`
driver-type %s
privilege-level admin
username %s
password %s
	`, driver, user, password)
}

func freeipmiConfigPipe(driver, user, password string) (string, error) {
	content := []byte(freeipmiConfig(driver, user, password))
	pipe := pipeName()
	err := syscall.Mkfifo(pipe, 0600)
	if err != nil {
		return "", err
	}

	go func(file string, data []byte) {
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, os.ModeNamedPipe)
		if err != nil {
			log.Errorf("Error opening pipe: %s", err)
		}
		if _, err := f.Write(data); err != nil {
			log.Errorf("Error writing config to pipe: %s", err)
		}
		f.Close()
	}(pipe, content)
	return pipe, nil
}

func freeipmiOutput(cmd string, rmcp *rmcpConfig, arg ...string) ([]byte, error) {
	args := []string{}

	if rmcp != nil {
		pipe, err := freeipmiConfigPipe("LAN_2_0", rmcp.user, rmcp.pass)
		if err != nil {
			return nil, err
		}
		defer os.Remove(pipe)

		rmcpArgs := []string{
			"--config-file", pipe,
			"-h", rmcp.host,
		}
		args = append(args, rmcpArgs...)
	}

	fqcmd := path.Join(*executablesPath, cmd)
	args = append(args, arg...)
	out, err := exec.Command(fqcmd, args...).CombinedOutput()
	if err != nil {
		log.Errorf("Error while calling %s for %s: %s", cmd, rmcp.host, out)
	}
	return out, err
}

func ipmiMonitoringOutput(rmcp *rmcpConfig) ([]byte, error) {
	return freeipmiOutput("ipmimonitoring", rmcp, "-Q", "--comma-separated-output", "--no-header-output", "--sdr-cache-recreate", "--session-timeout=5000")
}

func ipmiDCMIOutput(rmcp *rmcpConfig) ([]byte, error) {
	return freeipmiOutput("ipmi-dcmi", rmcp, "--get-system-power-statistics", "--session-timeout=5000")
}

func bmcInfoOutput(rmcp *rmcpConfig) ([]byte, error) {
	return freeipmiOutput("bmc-info", rmcp, "--get-device-id", "--session-timeout=5000")
}

func splitMonitoringOutput(impiOutput []byte, excludeSensorIds []int64) ([]sensorData, error) {
	var result []sensorData

	r := csv.NewReader(bytes.NewReader(impiOutput))
	fields, err := r.ReadAll()
	if err != nil {
		return result, err
	}

	for _, line := range fields {
		var data sensorData

		data.ID, err = strconv.ParseInt(line[0], 10, 64)
		if err != nil {
			return result, err
		}
		if contains(excludeSensorIds, data.ID) {
			continue
		}

		data.Name = line[1]
		data.Type = line[2]
		data.State = line[3]

		value := line[4]
		if value != "N/A" {
			data.Value, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return result, err
			}
		} else {
			data.Value = math.NaN()
		}

		data.Unit = line[5]
		data.Event = strings.Trim(line[6], "'")

		result = append(result, data)
	}
	return result, err
}

func getValue(ipmiOutput []byte, regex *regexp.Regexp) (string, error) {
	for _, line := range strings.Split(string(ipmiOutput), "\n") {
		match := regex.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		for i, name := range regex.SubexpNames() {
			if name != "value" {
				continue
			}
			return match[i], nil
		}
	}
	return "", fmt.Errorf("Could not find value in output: %s", string(ipmiOutput))
}

func getCurrentPowerConsumption(ipmiOutput []byte) (float64, error) {
	value, err := getValue(ipmiOutput, ipmiDCMICurrentPowerRegex)
	if err != nil {
		return -1, err
	}
	return strconv.ParseFloat(value, 64)
}

func getBMCInfoFirmwareRevision(ipmiOutput []byte) (string, error) {
	return getValue(ipmiOutput, bmcInfoFirmwareRevisionRegex)
}

func getBMCInfoManufacturerID(ipmiOutput []byte) (string, error) {
	return getValue(ipmiOutput, bmcInfoManufacturerIDRegex)
}

// Describe implements Prometheus.Collector.
func (c collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- sensorStateDesc
	ch <- sensorValueDesc
	ch <- fanSpeedDesc
	ch <- temperatureDesc
	ch <- powerConsumption
	ch <- bmcInfo
	ch <- upDesc
	ch <- durationDesc
}

func collectTypedSensor(ch chan<- prometheus.Metric, desc, stateDesc *prometheus.Desc, state float64, data sensorData) {
	ch <- prometheus.MustNewConstMetric(
		desc,
		prometheus.GaugeValue,
		data.Value,
		strconv.FormatInt(data.ID, 10),
		data.Name,
	)
	ch <- prometheus.MustNewConstMetric(
		stateDesc,
		prometheus.GaugeValue,
		state,
		strconv.FormatInt(data.ID, 10),
		data.Name,
	)
}

func collectGenericSensor(ch chan<- prometheus.Metric, state float64, data sensorData) {
	ch <- prometheus.MustNewConstMetric(
		sensorValueDesc,
		prometheus.GaugeValue,
		data.Value,
		strconv.FormatInt(data.ID, 10),
		data.Name,
		data.Type,
	)
	ch <- prometheus.MustNewConstMetric(
		sensorStateDesc,
		prometheus.GaugeValue,
		state,
		strconv.FormatInt(data.ID, 10),
		data.Name,
		data.Type,
	)
}

func (c collector) collectMonitoring(ch chan<- prometheus.Metric, rmcp *rmcpConfig) (int, error) {
	output, err := ipmiMonitoringOutput(rmcp)
	if err != nil {
		log.Errorf("Failed to collect ipmimonitoring data from %s: %s", rmcp.host, err)
		return 0, err
	}
	excludeIds := c.config.ExcludeSensorIDs()
	results, err := splitMonitoringOutput(output, excludeIds)
	if err != nil {
		log.Errorf("Failed to parse ipmimonitoring data from %s: %s", rmcp.host, err)
		return 0, err
	}
	for _, data := range results {
		var state float64

		switch data.State {
		case "Nominal":
			state = 0
		case "Warning":
			state = 1
		case "Critical":
			state = 2
		case "N/A":
			state = math.NaN()
		default:
			log.Errorf("Unknown sensor state: '%s'\n", data.State)
			state = math.NaN()
		}

		log.Debugf("Got values: %v\n", data)

		switch data.Unit {
		case "RPM":
			collectTypedSensor(ch, fanSpeedDesc, fanSpeedStateDesc, state, data)
		case "C":
			collectTypedSensor(ch, temperatureDesc, temperatureStateDesc, state, data)
		case "A":
			collectTypedSensor(ch, currentDesc, currentStateDesc, state, data)
		case "V":
			collectTypedSensor(ch, voltageDesc, voltageStateDesc, state, data)
		case "W":
			collectTypedSensor(ch, powerDesc, powerStateDesc, state, data)
		default:
			collectGenericSensor(ch, state, data)
		}
	}
	return 1, nil
}

func (c collector) collectDCMI(ch chan<- prometheus.Metric, rmcp *rmcpConfig) (int, error) {
	output, err := ipmiDCMIOutput(rmcp)
	if err != nil {
		log.Debugf("Failed to collect ipmi-dcmi data from %s: %s", rmcp.host, err)
		return 0, err
	}
	currentPowerConsumption, err := getCurrentPowerConsumption(output)
	if err != nil {
		log.Errorf("Failed to parse ipmi-dcmi data from %s: %s", rmcp.host, err)
		return 0, err
	}
	ch <- prometheus.MustNewConstMetric(
		powerConsumption,
		prometheus.GaugeValue,
		currentPowerConsumption,
	)
	return 1, nil
}

func (c collector) collectBmcInfo(ch chan<- prometheus.Metric, rmcp *rmcpConfig) (int, error) {
	output, err := bmcInfoOutput(rmcp)
	if err != nil {
		log.Debugf("Failed to collect bmc-info data from %s: %s", rmcp.host, err)
		return 0, err
	}
	firmwareRevision, err := getBMCInfoFirmwareRevision(output)
	if err != nil {
		log.Errorf("Failed to parse bmc-info data from %s: %s", rmcp.host, err)
		return 0, err
	}
	manufacturerID, err := getBMCInfoManufacturerID(output)
	if err != nil {
		log.Errorf("Failed to parse bmc-info data from %s: %s", rmcp.host, err)
		return 0, err
	}
	ch <- prometheus.MustNewConstMetric(
		bmcInfo,
		prometheus.GaugeValue,
		1,
		firmwareRevision, manufacturerID,
	)
	return 1, nil
}

func (c collector) markCollectorsUp(ch chan<- prometheus.Metric, bmc, dcmi, ipmi int) {
	ch <- prometheus.MustNewConstMetric(
		upDesc,
		prometheus.GaugeValue,
		float64(bmc),
		"bmc",
	)
	ch <- prometheus.MustNewConstMetric(
		upDesc,
		prometheus.GaugeValue,
		float64(dcmi),
		"dcmi",
	)
	ch <- prometheus.MustNewConstMetric(
		upDesc,
		prometheus.GaugeValue,
		float64(ipmi),
		"ipmi",
	)
}

// Collect implements Prometheus.Collector.
func (c collector) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	defer func() {
		duration := time.Since(start).Seconds()
		log.Debugf("Scrape of target %s took %f seconds.", targetName(c.target), duration)
		ch <- prometheus.MustNewConstMetric(
			durationDesc,
			prometheus.GaugeValue,
			duration,
		)
	}()

	rmcp := (*rmcpConfig)(nil)

	if !targetIsLocal(c.target) {
		creds, err := c.config.CredentialsForTarget(c.target)
		if err != nil {
			log.Errorf("No credentials available for target %s.", c.target)
			c.markCollectorsUp(ch, 0, 0, 0)
			return
		}
		rmcp = &rmcpConfig{
			host: c.target,
			user: creds.User,
			pass: creds.Password,
		}
	}

	ipmiUp, _ := c.collectMonitoring(ch, rmcp)
	dcmiUp, _ := c.collectDCMI(ch, rmcp)
	bmcUp, _ := c.collectBmcInfo(ch, rmcp)

	c.markCollectorsUp(ch, bmcUp, dcmiUp, ipmiUp)
}

func contains(s []int64, elm int64) bool {
	for _, a := range s {
		if a == elm {
			return true
		}
	}
	return false
}

func targetName(target string) string {
	if targetIsLocal(target) {
		return "[local]"
	}
	return target
}

func targetIsLocal(target string) bool {
	return target == targetLocal
}
