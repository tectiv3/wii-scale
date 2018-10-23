package wiiboard

import (
	"bufio"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	evdev "github.com/gvalkov/golang-evdev"
	"github.com/pkg/errors"
)

const (
	deviceglob      = "/dev/input/event*"
	nintendoVendor  = 0x057E
	wiiBoardProduct = 0x0306
)

var logrus *log.Logger

func init() {
	logrus = log.New()
	// redirect Go standard log library calls to logrus writer
	stdlog.SetFlags(0)
	stdlog.SetOutput(logrus.Writer())
	stdlog.SetFlags(stdlog.LstdFlags | stdlog.Lshortfile)
	logrus.Out = os.Stdout

	logrus.Level, _ = log.ParseLevel("debug")
	log.SetLevel(logrus.Level)

	f, err := os.OpenFile("/tmp/wii.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		logrus.Error(err)
		return
	}

	log.SetOutput(f)
	stdlog.SetOutput(f)
	logrus.Out = f

	logrus.Info("Started wii-scale")
}

// wiiBoard is the currently connected wiiboard connection
type wiiBoard struct {
	Weights chan float64

	conn        *evdev.InputDevice
	batteryPath string

	calibrating bool
	mux         *sync.RWMutex
	lastWeight  float64
	events      chan Event
}

// Event represents various pressure point generated by the wii balance board
type Event struct {
	TopLeft     int32
	TopRight    int32
	BottomRight int32
	BottomLeft  int32
	Total       float64
	Button      bool
}

func New() wiiBoard {
	return wiiBoard{
		mux:     &sync.RWMutex{},
		events:  make(chan Event),
		Weights: make(chan float64),
	}
}

// Detect enables picking first connected WiiBoard on the system
func (w *wiiBoard) Detect() error {
	devices, err := evdev.ListInputDevices(deviceglob)
	if err != nil {
		return errors.Wrapf(err, "couldn't list input device on system")
	}

	for _, dev := range devices {
		if dev.Vendor != nintendoVendor || dev.Product != wiiBoardProduct {
			continue
		}

		// look for battery path
		var batteryPath string
		f, err := os.Open("/proc/bus/input/devices")
		if err != nil {
			return errors.Wrapf(err, "couldn't find input device list file")
		}
		defer f.Close()

		boardStenza := false
		matchBoard := fmt.Sprintf("Vendor=0%x Product=0%x", nintendoVendor, wiiBoardProduct)
		re := regexp.MustCompile("S: Sysfs=(.*)")
		scanner := bufio.NewScanner(f)

		for scanner.Scan() {
			t := scanner.Text()
			if t == "" && boardStenza {
				return errors.New("didn't find expected sys location in input device list file")
			}
			if strings.Contains(t, matchBoard) {
				boardStenza = true
			}
			if !boardStenza {
				continue
			}
			res := re.FindStringSubmatch(t)
			if len(res) < 2 {
				continue
			}
			m, err := filepath.Glob("/sys" + res[1] + "/device/power_supply/*/capacity")
			if err != nil || len(m) != 1 {
				return errors.New("didn't find expected battery capacity location")
			}
			batteryPath = m[0]
			break
		}

		if err := scanner.Err(); err != nil {
			return errors.Wrapf(err, "error reading input device list file")
		}

		w.conn = dev
		w.batteryPath = batteryPath
		return nil
	}

	return errors.New("Didn't find WiiBoard")
}

// Listen start sending events on Events property of the board
// Necessary before doing any operation, like calibrating
func (w *wiiBoard) Listen() {
	curEvent := Event{}
	_ = curEvent
	for {
		events, err := w.conn.Read()
		if err != nil {
			logrus.Error("Reading event error: %v", err)
			// board disconnected, exit
			os.Exit(0)
		}
		// logrus.Debugf("Got %d events, ranging...", len(events))
		if len(events) < 5 {
			// skip incomplete events
			continue
		}
		for _, e := range events {
			// logrus.Debug(e.String())
			switch e.Type {
			case evdev.EV_SYN:
				w.mux.RLock()
				if !w.calibrating {
					// check for weights deviation, if deviation is big enough
					// recalibrate and send new weight
					if math.Abs(float64(curEvent.Total)-w.lastWeight)/w.lastWeight > 0.05 {
						w.mux.RUnlock()
						go w.sendMeanTotal()
						curEvent = Event{}
						continue
					}

					if curEvent.Total < 200 {
						w.mux.RUnlock()
						curEvent = Event{}
						continue
					}
				}
				w.mux.RUnlock()

				// send current event and reset it.
				// Don't block on sending if other side is slower than input events
				select {
				case w.events <- curEvent:
				default:
				}
				curEvent = Event{}

			// pressure point
			case evdev.EV_ABS:
				switch e.Code {
				case evdev.ABS_HAT0Y:
					curEvent.BottomRight = e.Value
				case evdev.ABS_HAT1Y:
					curEvent.BottomLeft = e.Value
				case evdev.ABS_HAT0X:
					curEvent.TopRight = e.Value
				case evdev.ABS_HAT1X:
					curEvent.TopLeft = e.Value
				default:
					if m, exists := evdev.ByEventType[int(e.Type)]; exists {
						logrus.Infof("Unexpected event code: %s", m[int(e.Code)])
					} else {
						logrus.Infof("Unexpected unknown event code: %d", e.Code)
					}
					continue
				}
				curEvent.Total = float64(curEvent.TopLeft + curEvent.TopRight + curEvent.BottomLeft + curEvent.BottomRight)
			// main button
			case evdev.EV_KEY:
				if e.Code != 304 {
					logrus.WithField("e", e).Infof("Unexpected event code: %d", e.Code)
					continue
				}
				curEvent.Button = true
			default:
				logrus.WithField("e", e).Infof("Unexpected unknown event type: %d", e.Type)
			}
		}
	}
}

// take 50 measures. calculate median. send it
func (w *wiiBoard) sendMeanTotal() {
	w.mux.RLock()
	if w.calibrating {
		w.mux.RUnlock()
		return
	}
	w.mux.RUnlock()
	w.mux.Lock()
	w.lastWeight = 0
	w.calibrating = true
	w.mux.Unlock()

	// logrus.Debug("Calibrating...")
	measureTime := time.Now().Add(3 * time.Second)

	var topLeft, topRight, bottomRight, bottomLeft int32
	lastWeight := int32(0)
	var n int32
	for {
		// We want at least 100 valid measures over 3 seconds
		if time.Now().After(measureTime) && n > 100 {
			break
		}
		select {
		case e := <-w.events:
			newWeight := e.TopLeft + e.TopRight + e.BottomRight + e.BottomLeft
			// skips if one sensor sends 0, as we want an equilibrium state, we skip this invalid measure
			if e.TopLeft == 0 || e.TopRight == 0 || e.BottomLeft == 0 || e.BottomRight == 0 {
				continue
			}

			// reset if weight is too light or changed by more than 20%: not stable yet!
			if newWeight < 100 || math.Abs(float64(lastWeight-newWeight))/float64(newWeight) > 0.2 {
				topLeft = 0
				topRight = 0
				bottomRight = 0
				bottomLeft = 0
				n = 0
				measureTime = time.Now().Add(3 * time.Second)
				lastWeight = newWeight
				continue
			}

			lastWeight = newWeight
			topLeft += e.TopLeft
			topRight += e.TopRight
			bottomRight += e.BottomRight
			bottomLeft += e.BottomLeft
			n++
		case <-time.After(5 * time.Second):
			// logrus.Debug("Canceled.")
			w.mux.Lock()
			w.calibrating = false
			w.mux.Unlock()
			return
		}

	}

	w.mux.Lock()
	w.lastWeight = float64((topLeft + topRight + bottomRight + bottomLeft) / n)
	w.calibrating = false
	// logrus.Debugf("Calibrated! %.2f", w.lastWeight)

	// send current weight.
	// Don't block on sending if other side is slower than input events
	select {
	case w.Weights <- w.lastWeight:
	default:
	}
	w.mux.Unlock()
}

// Battery returns current power level
func (w wiiBoard) Battery() (int, error) {
	b, err := ioutil.ReadFile(w.batteryPath)
	if err != nil {
		return 0, errors.Wrap(err, "couldn't read from board battery file")
	}
	battery, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, errors.Wrap(err, "didn't find an integer in battery capacity file")
	}
	return battery, nil
}
