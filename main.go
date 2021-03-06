// Currently everything for ether_housed is in the main package
// BUG: Split up stuff into a separate package?
package main

import (
	"fmt"
	"github.com/bmizerany/mc"
	"github.com/dustin/go-humanize"
	"log"
	"log/syslog"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const NUM_HOUSES = 8

type event struct {
	timestamp int64
	house_id  int64
	message   string
}

// common is a struct to store the global state and config
type common struct {
	lock       sync.RWMutex
	state      []bool
	api_key    []string
	target_mac []string
	last_seen  []int64
	events     []event
	mc         *mc.Conn
}

var Common = new(common)

// Get is a method on the common struct to safely read
// the state id of a particular house
func (c *common) Get(id int) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	d := c.state[id]
	return d
}

// Set will lock and set the state of a house
func (c *common) Set(id int, d bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.state[id] = d
	log.Printf("Saving into memcache...")
	state_value := strconv.Itoa(boolarraytoint(c.state))
	ocas := 0
	exp := 0
	flags := 0
	err := c.mc.Set("state", state_value, ocas, flags, exp)
	if err == nil {
		log.Printf("memcached Saved state: %08b", state_value)
	} else {
		log.Printf("Error saving state into memcache: ", err)
	}
}

func (c *common) LogEvent(house_id int64, message string) {
	ts := time.Now().Unix()
	e := event{ts, house_id, message}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.events = append(c.events, e)
	keep_start := 0
	max_items := 8 * 100 // houses * events
	cur_len := len(c.events)
	if cur_len > max_items {
		keep_start = cur_len - max_items
	}
	var max_age int64 = 7 * 24 * 60 * 60
	for i := keep_start; i < cur_len; i++ {
		event_age := ts - c.events[i].timestamp
		if event_age > max_age {
			keep_start = i + 1
		}
	}
	if keep_start > 0 {
		c.events = c.events[keep_start:]
	}
}

func (c *common) GetLog() []event {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.events
}

// load_existing_state pulls state from an external datastore
func load_existing_state() {
	if Common.mc != nil {
		log.Printf("Retrieving initial state from memcache...")
		val, cas, flags, err := Common.mc.Get("state")
		log.Printf(val, cas, flags, err)
		if err == nil {
			Common.state = stringtoboolarray(val)
			state_value := get_state_as_int()
			log.Printf("Done. Loaded state: %08b", state_value)
		} else {
			log.Printf("Error loading state from memcache, ", err)
			log.Printf("Loading a blank state instead")
			Common.state = []bool{false, false, false, false, false, false, false, false}
		}
	} else {
		log.Printf("Memcache not available. Defaulting to a blank state")
		Common.state = []bool{false, false, false, false, false, false, false, false}
	}
	Common.last_seen = []int64{0, 0, 0, 0, 0, 0, 0, 0}
	Common.events = []event{}
	return
}

// load_target_macs gets the config for which MAC addresses are associated with each house
// from the environment
func load_target_macs() {
	Common.target_mac = []string{"", "", "", "", "", "", "", ""}
	for i := 0; i < NUM_HOUSES; i++ {
		Common.target_mac[i] = strings.TrimSpace(os.Getenv("MAC" + strconv.Itoa(i)))
		if Common.target_mac[i] == "" {
			log.Println("WARNING: Didn't get an MAC for " + strconv.Itoa(i) + ".")
		}
		log.Println("INFO: MAC for " + strconv.Itoa(i) + " is " + Common.target_mac[i])
	}
}

// load_api_keys reads in an APIKEYN environment variable for each houseid
func load_api_keys() {
	Common.api_key = []string{"", "", "", "", "", "", "", ""}
	for i := 0; i < NUM_HOUSES; i++ {
		Common.api_key[i] = strings.TrimSpace(os.Getenv("APIKEY" + strconv.Itoa(i)))
		if Common.api_key[i] == "" {
			log.Println("WARNING: Didn't get an API key for " + strconv.Itoa(i) + ".")
		}
		log.Println("INFO: API Key for " + strconv.Itoa(i) + " is " + Common.api_key[i])
	}
}

func setup_logging() {
	logwriter, e := syslog.New(syslog.LOG_NOTICE, "ether_housed")
	if e == nil {
		log.SetOutput(logwriter)
	}
}

var chttp = http.NewServeMux()

func initialize_memcached() *mc.Conn {
	servers := os.Getenv("MEMCACHEDCLOUD_SERVERS")
	if servers != "" {
		log.Println("Read MEMCACHECLOUD  config from env")
	} else {
		log.Println("Failed to read MEMCACHEDCLOUD Variables. Defaulting to localhost")
		servers = "127.0.0.1:11211"
	}
	mc, err := mc.Dial("tcp", servers)
	if err != nil {
		log.Println("Memcache isn't available. Error: ", err)
	} else {
		log.Println("Memcache is available. Yay!")
	}
	username := os.Getenv("MEMCACHEDCLOUD_USERNAME")
	password := os.Getenv("MEMCACHEDCLOUD_PASSWORD")
	if username != "" && password != "" {
		log.Println("MEMCACEDCLOUT Auth variables present. Trying to use SASL...")
		err = mc.Auth(username, password)
		if err == nil {
			log.Println("Memcached SASL Auth Worked")
		} else {
			log.Println("Memcached SASL Auth failed: ", err)
		}
	} else {
		log.Println("No memcached auth variables available. Skipping SASL")
	}
	set_err := mc.Set("test_key", "0", 0, 0, 3600)
	if set_err == nil {
		log.Println("Setting a test key in memcache worked.")
	} else {
		log.Println("Setting a memcache key did not work. ", set_err)
	}
	return mc
}

func main() {
	Common.mc = initialize_memcached()

	load_existing_state()
	load_api_keys()
	load_target_macs()
	http.HandleFunc("/", handle_usage)
	http.HandleFunc("/on", handle_turn_on)
	http.HandleFunc("/off", handle_turn_off)
	http.HandleFunc("/state", handle_state)
	http.HandleFunc("/info", handle_info)
	http.HandleFunc("/target_mac", handle_target_mac)
	http.HandleFunc("/log", handle_log)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
		log.Print("No PORT variable. Defaulting to 3000")
	}
	log.Print("listening on " + port + "...")
	chttp.Handle("/", http.FileServer(http.Dir("./public")))
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		panic(err)
	}
}

// record_last_seen saves a timestamp of when a house checks in
func record_last_seen(id int64) {
	Common.last_seen[int(id)] = time.Now().Unix()
}

func handle_usage(res http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		chttp.ServeHTTP(res, req)
	} else {
		msg := "Welcome to ether_house.\n"
		msg += "Source code: https://github.com/solarkennedy/ether_housed \n"
		msg += "Client code: https://github.com/solarkennedy/ether_house \n"
		fmt.Fprintln(res, msg)
		log.Println("200: " + req.URL.Path)
	}
}

// boolarraytoint converts our array of booleans into a binary representation for http output
func boolarraytoint(bool_array []bool) (out int) {
	for index, value := range bool_array {
		if value == true {
			out += int(int64(math.Exp2(float64(index))))
		}
	}
	return out
}

// stringtoboolarray takes a stored string in memcache and gets it to the format we need for operation
func stringtoboolarray(in string) (output []bool) {
	var x uint
	theint, _ := strconv.Atoi(in)
	output = []bool{false, false, false, false, false, false, false, false}
	for x = 0; x < 8; x++ {
		output[x] = bitRead(uint8(theint), x)
		//output[((x - 7) % 8)] = bitRead(uint8(theint), x)
	}
	return output
}

func bitRead(value uint8, bit uint) bool {
	return inttobool((int(value) >> bit) & 0x01)
}

func inttobool(x int) bool {
	if x == 1 {
		return true
	} else {
		return false
	}
}

func mactobinary(mac string) (output []byte) {
	output, err := net.ParseMAC(mac)
	if err != nil {
		log.Printf("Error parsing mac: %v, output: %v, error: %v", mac, output, err)
	}
	return output
}

func get_state_as_int() (state_int int) {
	Common.lock.Lock()
	defer Common.lock.Unlock()
	return boolarraytoint(Common.state)
}

func handle_state(res http.ResponseWriter, req *http.Request) {
	house_id := validate_key(res, req)
	if house_id < 0 {
		return
	}
	state_value := get_state_as_int()
	tmp_bytes := []byte{byte(state_value)}
	res.Header().Set("Content-Type", "application/octet-stream")
	res.Write(tmp_bytes)
	log.Printf("200: Current State: %08b", state_value)
	record_last_seen(house_id)
}

func last_seen_output(last_seen []int64, now time.Time) (output string) {
	for x := 0; x < 8; x++ {
		output += fmt.Sprintf("House %d: ", x)
		if last_seen[x] == 0 {
			output += "Never"
		} else {
			last_seen_time := time.Unix(last_seen[x], 0)
			output += last_seen_time.String()
			output += fmt.Sprintf(" (%s)", humanize.Time(last_seen_time))
		}
		output += "\n"
	}
	return output
}

func handle_info(res http.ResponseWriter, req *http.Request) {
	house_id := validate_key(res, req)
	if house_id < 0 {
		return
	}
	state_value := get_state_as_int()
	target_mac := Common.target_mac[house_id]
	fmt.Fprintf(res, "Hi!!! Curious about how this works? Here is some debug info.\n\n\n")
	fmt.Fprintf(res, "Information on house_id: %v\n", house_id)
	fmt.Fprintf(res, "Current state: "+"%08b (%v)\n", state_value, state_value)
	fmt.Fprintf(res, "Target MAC Address: %v\n\n\n", target_mac)
	last_seen_output := last_seen_output(Common.last_seen, time.Now())
	fmt.Fprintf(res, "Last seen: \n%s\n\n", last_seen_output)
	log_text := get_logs(house_id)
	if log_text == "" {
		log_text = "(None yet)\n"
	}
	fmt.Fprintf(res, "Logs for this house:\n%s\n\n", log_text)
	fmt.Fprintf(res, "Server Source code: https://github.com/solarkennedy/ether_housed \n")
	fmt.Fprintf(res, "Client code: https://github.com/solarkennedy/ether_house \n")
	log.Printf("200: /info for %v", house_id)
}

func handle_target_mac(res http.ResponseWriter, req *http.Request) {
	house_id := validate_key(res, req)
	if house_id < 0 {
		return
	}
	target_mac := Common.target_mac[house_id]
	target_mac_binary := mactobinary(target_mac)
	res.Header().Set("Content-Type", "application/octet-stream")
	res.Write(target_mac_binary)
	log.Printf("200: target_mac: %v ", target_mac)
	Common.LogEvent(house_id, "handle_target_mac")
}

func handle_turn_on(res http.ResponseWriter, req *http.Request) {
	house_id := validate_key(res, req)
	if house_id < 0 {
		return
	}
	Common.Set(int(house_id), true)
	fmt.Fprintf(res, "Turned on %v", house_id)
	log.Printf("200: turn_on: %v", house_id)
	Common.LogEvent(house_id, "handle_turn_on")
}

func handle_turn_off(res http.ResponseWriter, req *http.Request) {
	house_id := validate_key(res, req)
	if house_id < 0 {
		return
	}
	Common.Set(int(house_id), false)
	fmt.Fprintf(res, "Turned off %v", house_id)
	log.Printf("200: turn_off: %v", house_id)
	Common.LogEvent(house_id, "handle_turn_off")
}

func get_logs(house_id int64) string {
	output := ""
	events := Common.GetLog()
	for _, e := range events {
		if e.house_id != house_id {
			continue
		}
		output += time.Unix(e.timestamp, 0).String()
		output += fmt.Sprintf(": House %d: ", e.house_id)
		output += e.message
		output += "\n"
	}
	return output
}

func handle_log(res http.ResponseWriter, req *http.Request) {
	house_id := validate_key(res, req)
	if house_id < 0 {
		return
	}
	logs_text := get_logs(house_id)
	fmt.Fprintf(res, logs_text)
	log.Printf("200: /log for %v", house_id)
}

// validate_key ensures that the provided key matches the one stored for that house_id
func validate_key(res http.ResponseWriter, req *http.Request) (house_id int64) {
	url := req.URL
	query := url.Query()
	api_key := query.Get("api_key")
	house_id_string := query.Get("id")
	house_id, _ = strconv.ParseInt(house_id_string, 0, 64)
	if Common.api_key[house_id] != api_key {
		http.Error(res, "403 Forbidden : you can't access this resource.", 403)
		log.Printf("403: %s from %v, using api key %v", url.Path, house_id, api_key)
		return -1
	}
	return house_id
}
