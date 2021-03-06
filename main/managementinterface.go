/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New"" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	managementinterface.go: Web interfaces (JSON and websocket), web server for web interface HTML.
*/

package main

import (
	"database/sql"
	"github.com/elgs/gosqljson"
	_ "github.com/mattn/go-sqlite3"
	"encoding/hex"
	"encoding/json"
	"fmt"
	humanize "github.com/dustin/go-humanize"
	"golang.org/x/net/websocket"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"text/template"
	"time"
	"strconv"
)

type SettingMessage struct {
	Setting string `json:"setting"`
	Value   bool   `json:"state"`
}

// Weather updates channel.
var weatherUpdate *uibroadcaster
var trafficUpdate *uibroadcaster

/*
	Tables in the SQLite database that can be queried as part of the flight logging API
*/
var tables = map[string]string{
	"flights": "startup",
	"status": "status",
	"uat": "messages",
	"es": "es_messages",
	"ownship": "mySituation",
	"events": "events",
	"traffic": "traffic"}


/*
	The /weather websocket starts off by sending the current buffer of weather messages, then sends updates as they are received.
*/
func handleWeatherWS(conn *websocket.Conn) {
	// Subscribe the socket to receive updates.
	weatherUpdate.AddSocket(conn)

	// Connection closes when function returns. Since uibroadcast is writing and we don't need to read anything (for now), just keep it busy.
	for {
		buf := make([]byte, 1024)
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
		if buf[0] != 0 { // Dummy.
			continue
		}
		time.Sleep(1 * time.Second)
	}
}

// Works just as weather updates do.

func handleTrafficWS(conn *websocket.Conn) {
	trafficMutex.Lock()
	for _, traf := range traffic {
		if !traf.Position_valid { // Don't send unless a valid position exists.
			continue
		}
		trafficJSON, _ := json.Marshal(&traf)
		conn.Write(trafficJSON)
	}
	// Subscribe the socket to receive updates.
	trafficUpdate.AddSocket(conn)
	trafficMutex.Unlock()

	// Connection closes when function returns. Since uibroadcast is writing and we don't need to read anything (for now), just keep it busy.
	for {
		buf := make([]byte, 1024)
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
		if buf[0] != 0 { // Dummy.
			continue
		}
		time.Sleep(1 * time.Second)
	}
}

func handleStatusWS(conn *websocket.Conn) {
	//	log.Printf("Web client connected.\n")

	timer := time.NewTicker(1 * time.Second)
	for {
		// The below is not used, but should be if something needs to be streamed from the web client ever in the future.
		/*		var msg SettingMessage
				err := websocket.JSON.Receive(conn, &msg)
				if err == io.EOF {
					break
				} else if err != nil {
					log.Printf("handleStatusWS: %s\n", err.Error())
				} else {
					// Use 'msg'.
				}
		*/

		// Send status.
		<-timer.C
		update, _ := json.Marshal(&globalStatus)
		_, err := conn.Write(update)

		if err != nil {
			//			log.Printf("Web client disconnected.\n")
			break
		}
	}
}

func handleSituationWS(conn *websocket.Conn) {
	timer := time.NewTicker(100 * time.Millisecond)
	for {
		<-timer.C
		situationJSON, _ := json.Marshal(&mySituation)
		_, err := conn.Write(situationJSON)

		if err != nil {
			break
		}

	}

}

func handleReplayWS(conn *websocket.Conn) {
	timer := time.NewTicker(500 * time.Millisecond)
	for {
		<-timer.C
		replayJSON, _ := json.Marshal(&replayStatus)
		_, err := conn.Write(replayJSON)

		if err != nil {
			break
		}

	}

}

// AJAX call - /getStatus. Responds with current global status
// a webservice call for the same data available on the websocket but when only a single update is needed
func handleStatusRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	statusJSON, _ := json.Marshal(&globalStatus)
	fmt.Fprintf(w, "%s\n", statusJSON)
}

// AJAX call - /getSituation. Responds with current situation (lat/lon/gdspeed/track/pitch/roll/heading/etc.)
func handleSituationRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	situationJSON, _ := json.Marshal(&mySituation)
	fmt.Fprintf(w, "%s\n", situationJSON)
}

// AJAX call - /getTowers. Responds with all ADS-B ground towers that have sent messages that we were able to parse, along with its stats.
func handleTowersRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)

	ADSBTowerMutex.Lock()
	towersJSON, err := json.Marshal(&ADSBTowers)
	if err != nil {
		log.Printf("Error sending tower JSON data: %s\n", err.Error())
	}
	// for testing purposes, we can return a fixed reply
	// towersJSON = []byte(`{"(38.490880,-76.135554)":{"Lat":38.49087953567505,"Lng":-76.13555431365967,"Signal_strength_last_minute":100,"Signal_strength_max":67,"Messages_last_minute":1,"Messages_total":1059},"(38.978698,-76.309276)":{"Lat":38.97869825363159,"Lng":-76.30927562713623,"Signal_strength_last_minute":495,"Signal_strength_max":32,"Messages_last_minute":45,"Messages_total":83},"(39.179285,-76.668413)":{"Lat":39.17928457260132,"Lng":-76.66841268539429,"Signal_strength_last_minute":50,"Signal_strength_max":24,"Messages_last_minute":1,"Messages_total":16},"(39.666309,-74.315300)":{"Lat":39.66630935668945,"Lng":-74.31529998779297,"Signal_strength_last_minute":9884,"Signal_strength_max":35,"Messages_last_minute":4,"Messages_total":134}}`)
	fmt.Fprintf(w, "%s\n", towersJSON)
	ADSBTowerMutex.Unlock()
}

// AJAX call - /getSatellites. Responds with all GNSS satellites that are being tracked, along with status information.
func handleSatellitesRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	satelliteMutex.Lock()
	satellitesJSON, err := json.Marshal(&Satellites)
	if err != nil {
		log.Printf("Error sending GNSS satellite JSON data: %s\n", err.Error())
	}
	fmt.Fprintf(w, "%s\n", satellitesJSON)
	satelliteMutex.Unlock()
}

// AJAX call - /getSettings. Responds with all stratux.conf data.
func handleSettingsGetRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	settingsJSON, _ := json.Marshal(&globalSettings)
	fmt.Fprintf(w, "%s\n", settingsJSON)
}

// AJAX call - /setSettings. receives via POST command, any/all stratux.conf data.
func handleSettingsSetRequest(w http.ResponseWriter, r *http.Request) {
	// define header in support of cross-domain AJAX
	setNoCache(w)
	setJSONHeaders(w)
	w.Header().Set("Access-Control-Allow-Method", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

	// for an OPTION method request, we return header without processing.
	// this insures we are recognized as supporting cross-domain AJAX REST calls
	if r.Method == "POST" {
		// raw, _ := httputil.DumpRequest(r, true)
		// log.Printf("handleSettingsSetRequest:raw: %s\n", raw)

		decoder := json.NewDecoder(r.Body)
		for {
			var msg map[string]interface{} // support arbitrary JSON

			err := decoder.Decode(&msg)
			if err == io.EOF {
				break
			} else if err != nil {
				log.Printf("handleSettingsSetRequest:error: %s\n", err.Error())
			} else {
				for key, val := range msg {
					// log.Printf("handleSettingsSetRequest:json: testing for key:%s of type %s\n", key, reflect.TypeOf(val))
					switch key {
					case "UAT_Enabled":
						globalSettings.UAT_Enabled = val.(bool)
					case "ES_Enabled":
						globalSettings.ES_Enabled = val.(bool)
					case "Ping_Enabled":
						globalSettings.Ping_Enabled = val.(bool)
					case "GPS_Enabled":
						globalSettings.GPS_Enabled = val.(bool)
					case "AHRS_Enabled":
						globalSettings.AHRS_Enabled = val.(bool)
					case "DEBUG":
						globalSettings.DEBUG = val.(bool)
					case "DisplayTrafficSource":
						globalSettings.DisplayTrafficSource = val.(bool)
					case "ReplayLog":
						v := val.(bool)
						if v != globalSettings.ReplayLog { // Don't mark the files unless there is a change.
							globalSettings.ReplayLog = v
						}
					case "PPM":
						globalSettings.PPM = int(val.(float64))
					case "FlightLogLevel":
						globalSettings.FlightLogLevel = int(val.(float64))
					case "Baud":
						if serialOut, ok := globalSettings.SerialOutputs["/dev/serialout0"]; ok { //FIXME: Only one device for now.
							newBaud := int(val.(float64))
							if newBaud == serialOut.Baud { // Same baud rate. No change.
								continue
							}
							log.Printf("changing /dev/serialout0 baud rate from %d to %d.\n", serialOut.Baud, newBaud)
							serialOut.Baud = newBaud
							// Close the port if it is open.
							if serialOut.serialPort != nil {
								log.Printf("closing /dev/serialout0 for baud rate change.\n")
								serialOut.serialPort.Close()
								serialOut.serialPort = nil
							}
							globalSettings.SerialOutputs["/dev/serialout0"] = serialOut
						}
					case "WatchList":
						globalSettings.WatchList = val.(string)
					case "OwnshipModeS":
						// Expecting a hex string less than 6 characters (24 bits) long.
						if len(val.(string)) > 6 { // Too long.
							continue
						}
						// Pad string, must be 6 characters long.
						vals := strings.ToUpper(val.(string))
						for len(vals) < 6 {
							vals = "0" + vals
						}
						hexn, err := hex.DecodeString(vals)
						if err != nil { // Number not valid.
							log.Printf("handleSettingsSetRequest:OwnshipModeS: %s\n", err.Error())
							continue
						}
						globalSettings.OwnshipModeS = fmt.Sprintf("%02X%02X%02X", hexn[0], hexn[1], hexn[2])
					default:
						log.Printf("handleSettingsSetRequest:json: unrecognized key:%s\n", key)
					}
				}
				saveSettings()
			}
		}

		// while it may be redundent, we return the latest settings
		settingsJSON, _ := json.Marshal(&globalSettings)
		fmt.Fprintf(w, "%s\n", settingsJSON)
	}
}

func handleShutdownRequest(w http.ResponseWriter, r *http.Request) {
	syscall.Sync()
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}

func doReboot() {
	syscall.Sync()
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}

func handleRebootRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	w.Header().Set("Access-Control-Allow-Method", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	go delayReboot()
}

// AJAX call - /getClients. Responds with all connected clients.
func handleClientsGetRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	clientsJSON, _ := json.Marshal(&outSockets)
	fmt.Fprintf(w, "%s\n", clientsJSON)
}



func openDatabase() (db *sql.DB, err error) {

	db, err = sql.Open("sqlite3", dataLogFilef)
	if err != nil {
		log.Printf("sql.Open(): %s\n", err.Error())
	}

	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		log.Printf("db.Exec('PRAGMA journal_mode=WAL') err: %s\n", err.Error())
	}
	_, err = db.Exec("PRAGMA synchronous=OFF")
	if err != nil {
		log.Printf("db.Exec('PRAGMA journal_mode=WAL') err: %s\n", err.Error())
	}
	
	return db, err
}

func getCount(sql string, db *sql.DB) (count int64) {

	rows, err := db.Query(sql)
	defer rows.Close()
	if (err != nil) {
		return 0
	}
	for rows.Next() {
		err := rows.Scan(&count)
		if err != nil {
			return 0
		}
	}

	return count	
}

/*
	handleFlightLogFlightsRequest(): returns a list of flights as JSON. Data is returned
	in descending (most recent first) timestamp order. If more than 100 flights are 
	stored, the system will always return the 100 most recent unless an offset value is 
	passed.
*/
func handleFlightLogFlightsRequest(args []string, w http.ResponseWriter, r *http.Request) {
	
	db, err := openDatabase()
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	defer db.Close()
	
	var offset int
	if (len(args) > 0) {
		offset, err = strconv.Atoi(args[0])
		if (err != nil) {
			http.Error(w, "Invalid page value", http.StatusBadRequest)
			return
		}
		// page size is 10 records
		offset = (offset - 1) * 10
	}
	
	var count int64 
	count = getCount("SELECT COUNT(*) FROM startup WHERE duration > 1 AND distance > 1 AND ((max_alt - start_alt) > 350);", db)
	
	sql := fmt.Sprintf("SELECT * FROM startup WHERE duration > 1 AND distance > 1 AND ((max_alt - start_alt) > 350) ORDER BY id DESC LIMIT 10 OFFSET %d;", offset);
    m, err := gosqljson.QueryDbToMapJSON(db, "any", sql)
    if err != nil {
    	http.Error(w, err.Error(), http.StatusBadRequest)
    	return
    }

	ret := fmt.Sprintf("{\"count\": %d, \"limit\": 10, \"offset\": %d, \"data\": %s}", count, offset, m)
	
	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "%s\n", ret)
}

/*
	handleFlightLogEventsRequest(): returns all events associated with a given flight as
	JSON. Events are returned in ascending (oldest first) timestamp order.
*/
func handleFlightLogEventsRequest(args []string, w http.ResponseWriter, r *http.Request) {
	
	db, err := openDatabase()
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	defer db.Close()
	
	if (len(args) < 1) {
		http.Error(w, "/flightlog/events requires a flight id parameter", http.StatusBadRequest)
    	return
	}
	
	flight, err := strconv.Atoi(args[0])
	if (err != nil) {
		http.Error(w, "Invalid flight ID value", http.StatusBadRequest)
    	return
	}
		
	var count int64 
	count = getCount(fmt.Sprintf("SELECT COUNT(*) FROM events WHERE startup_id = %d;", flight), db)
	
	sql := fmt.Sprintf("SELECT * FROM events WHERE startup_id = %d ORDER BY timestamp_id ASC LIMIT 1000;", flight);
    m, err := gosqljson.QueryDbToMapJSON(db, "any", sql)
    if err != nil {
    	http.Error(w, err.Error(), http.StatusBadRequest)
    	return
    }

	ret := fmt.Sprintf("{\"count\": %d, \"data\": %s}", count, m)
	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "%s\n", ret)
}

/*
	Generates and returns a KML file representing a given flight. 
	
	Somebody with some actual KML-fu help! This needs to show height above the ground
	and other cool stuff.
*/
func handleFlightLogKMLRequest(args []string, w http.ResponseWriter, r *http.Request) {

	fmt.Println("about to create KML file")
	
	db, err := openDatabase()
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	defer db.Close()
	
	if (len(args) < 1) {
		http.Error(w, "/flightlog/kml requires a flight id parameter", http.StatusBadRequest)
    	return
	}
	
	flight, err := strconv.Atoi(args[0])
	if (err != nil) {
		http.Error(w, "Invalid flight ID value", http.StatusBadRequest)
    	return
	}
	
	fmt.Printf("creating KML file for flight %d\n", flight)
	
	var fname, fpath string
	fname = fmt.Sprintf("flight_%d_track.kml", flight)
	
	fmt.Printf("filename will be %s\n", fname)
	
	if (globalStatus.HardwareBuild == "FlightBox") {
		fpath = fmt.Sprintf("/root/log/%s", fname)
	} else {
		fpath = fmt.Sprintf("/var/log/%s", fname)
	}
	
	fmt.Printf("file path is %s\n", fpath)
	
	f, err := os.Create(fpath)
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	
	header := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<kml xmlns=\"http://www.opengis.net/kml/2.2\" xmlns:gx=\"http://www.google.com/kml/ext/2.2\">\n<Folder>\n\t<Placemark>\n\t\t<gx:Track>\n"
	header += "\t\t\t<altitudeMode>absolute</altitudeMode>\n"
	
	f.WriteString(header)
	
	// generate all the where's and the coords here
	var sql string
	var stime, ktime, otime string
	var itime time.Time
	var lat, lng, alt float64
	
	sql = fmt.Sprintf("SELECT GPSTime FROM mySituation WHERE startup_id = %d ORDER BY timestamp_id ASC;", flight)
	whenrows, err := db.Query(sql)
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	
	for whenrows.Next() {
		err := whenrows.Scan(&stime)
		if (err != nil) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
    		return
		}
		// 2010-05-28T02:02:44Z
		// 2006-01-02T15:04:05Z
		itime, _ = time.Parse("2006-01-02 15:04:05 +0000 MST", stime)
		ktime = itime.Format("2006-01-02T15:04:05Z")
		otime := fmt.Sprintf("\t\t\t<when>%s</when>\n", ktime)
		f.WriteString(otime)
	}
	whenrows.Close()
	
	fmt.Println("wrote out when values for KML")
	
	sql = fmt.Sprintf("SELECT Lat, Lng, Alt FROM mySituation WHERE startup_id = %d ORDER BY timestamp_id ASC;", flight)
	whererows, err := db.Query(sql)
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	for whererows.Next() {
		err = whererows.Scan(&lat, &lng, &alt)
		if (err != nil) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		otime = fmt.Sprintf("\t\t\t<gx:coord>%.6f %.6f %.3f</gx:coord>\n", lng, lat, (alt * 0.3048))
		f.WriteString(otime)
	}
	whererows.Close()
	
	fmt.Println("Wrote out where values for KML")
	
	footer := "\t\t</gx:Track>\n\t</Placemark>\n</Folder>\n</kml>"
	f.WriteString(footer)
	f.Close()
	
	fmt.Println("Closed KML file")
	
	http.Redirect(w, r, "/logs/stratux/" + fname, 303)
}

/*
	Generates and returns a CSV file representing a given flight. 
*/
func handleFlightLogCSVRequest(args []string, w http.ResponseWriter, r *http.Request) {

	db, err := openDatabase()
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	defer db.Close()
	
	if (len(args) < 1) {
		http.Error(w, "/flightlog/csv requires a flight id parameter", http.StatusBadRequest)
    	return
	}
	
	flight, err := strconv.Atoi(args[0])
	if (err != nil) {
		http.Error(w, "Invalid flight ID value", http.StatusBadRequest)
    	return
	}
	
	fmt.Printf("Flight ID: %d\n", flight)
}

func handleFlightLogDataRequest(args []string, w http.ResponseWriter, r *http.Request) {

	db, err := openDatabase()
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	defer db.Close()
	
	if (len(args) < 1) {
		http.Error(w, "/flightlog/data requires a flight id parameter", http.StatusBadRequest)
    	return
	}
	
	flight, err := strconv.Atoi(args[0])
	if (err != nil) {
		http.Error(w, "Invalid flight ID value", http.StatusBadRequest)
    	return
	}
	
	fmt.Printf("Flight ID: %d\n", flight)
}

func handleFlightLogDeleteRequest(args []string, w http.ResponseWriter, r *http.Request) {

	db, err := openDatabase()
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	defer db.Close()
	
	if (len(args) < 1) {
		http.Error(w, "/flightlog/delete requires a flight id parameter", http.StatusBadRequest)
    	return
	}
	
	flight, err := strconv.Atoi(args[0])
	if (err != nil) {
		http.Error(w, "Invalid flight ID value", http.StatusBadRequest)
    	return
	}
	
	sql := fmt.Sprintf("DELETE FROM events WHERE startup_id = %d;", flight);
	fmt.Println(sql)
	_, err = db.Exec(sql)
	if (err != nil) {
		fmt.Printf("Error deleting events: %s.\n", err.Error())
	}
	
	sql = fmt.Sprintf("DELETE FROM messages WHERE startup_id = %d;", flight);
	fmt.Println(sql)
	_, err = db.Exec(sql)
	if (err != nil) {
		fmt.Printf("Error deleting messages: %s.\n", err.Error())
	}
	
	sql = fmt.Sprintf("DELETE FROM es_messages WHERE startup_id = %d;", flight);
	fmt.Println(sql)
	_, err = db.Exec(sql)
	if (err != nil) {
		fmt.Printf("Error deleting es_messages: %s.\n", err.Error())
	}
	
	sql = fmt.Sprintf("DELETE FROM traffic WHERE startup_id = %d;", flight);
	fmt.Println(sql)
	_, err = db.Exec(sql)
	if (err != nil) {
		fmt.Printf("Error deleting traffic: %s.\n", err.Error())
	}
	
	sql = fmt.Sprintf("DELETE FROM mySituation WHERE startup_id = %d;", flight);
	fmt.Println(sql)
	_, err = db.Exec(sql)
	if (err != nil) {
		fmt.Printf("Error deleting mySituation: %s.\n", err.Error())
	}
	
	sql = fmt.Sprintf("DELETE FROM startup WHERE id = %d;", flight);
	fmt.Println(sql)
	_, err = db.Exec(sql)
	if (err != nil) {
		fmt.Printf("Error deleting flight: %s.\n", err.Error())
	}
	
	ret := fmt.Sprintf("{\"deleted\": %d}", flight)
	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "%s\n", ret)
}

func handleFlightLogPruneRequest(args []string, w http.ResponseWriter, r *http.Request) {

	db, err := openDatabase()
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	defer db.Close()
	
	if (len(args) < 1) {
		http.Error(w, "/flightlog/prune requires a flight id parameter", http.StatusBadRequest)
    	return
	}
	
	flight, err := strconv.Atoi(args[0])
	if (err != nil) {
		http.Error(w, "Invalid flight ID value", http.StatusBadRequest)
    	return
	}
	
	fmt.Printf("Flight ID: %d\n", flight)
}

func handleFlightLogPurgeRequest(args []string, w http.ResponseWriter, r *http.Request) {

	db, err := openDatabase()
	if (err != nil) {
    	http.Error(w, err.Error(), http.StatusInternalServerError)
    	return
	}
	defer db.Close()
	
}

func handleFlightLogRequest(w http.ResponseWriter, r *http.Request) {
	
	//flightlog/flights (returns all flights as JSON, most recent first)
	//flightlog/events/8 (returns all events for flight 8 as JSON in sequential order)
	//flightlog/kml/4 (generates a KML file for flight 4 and downloads it)
	//flightlog/csv/15 (generates a CSV file for flight 15 and downloads it)
	//flightlog/data/table/flight/limit/offset (select a dump of data from the log)
	//flightlog/delete/8 (delete data for flight 8)
	//flightlog/prune/8 (removes ADS-B messages and situation data but leaves flight log / events)
	//flightlog/purge (delete all flightlog data)
	
	path := strings.Split(r.URL.String(), "/")
	
	// everything starts with "/flightlog"
	if path[1] != "flightlog" {
		http.Error(w, "Missing flightlog prefix", http.StatusBadRequest)
    	return
	}
	
	// have to at least specify a table
	if len(path) < 3 {
		http.Error(w, "Not enough parameters", http.StatusBadRequest)
    	return
	}
	
	command := path[2]
	arguments := path[3:]
	
	switch command {
	case "flights":
		handleFlightLogFlightsRequest(arguments, w, r)
	case "events":
		handleFlightLogEventsRequest(arguments, w, r)
	case "kml":
		handleFlightLogKMLRequest(arguments, w, r)
	case "csv":
		handleFlightLogCSVRequest(arguments, w, r)
	case "data":
		handleFlightLogDataRequest(arguments, w, r)
	case "delete":
		handleFlightLogDeleteRequest(arguments, w, r)
	case "prune":
		handleFlightLogPruneRequest(arguments, w, r)
	case "purge":
		handleFlightLogPurgeRequest(arguments, w, r)
	default:
		http.Error(w, "Error - invalid FlightLog command.", http.StatusBadRequest)
	}
	
	return
}

func handleFlightLogReplayPlay(args []string, w http.ResponseWriter, r *http.Request) {

	var flight int64 = 0
	var speed int64 = 1
	var timestamp int64 = 0
	
	// next parameter is the flight ID. Use 0 to stop current playback
	flight, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		http.Error(w, "Error getting flight id from Play request.", http.StatusBadRequest)
		return
	}
	
	if len(args) > 1 {
		speed, err = strconv.ParseInt(args[1], 10, 64)
		if (err != nil) {
			http.Error(w, "Error getting speed from Play request.", http.StatusBadRequest)
			return
		}
	}
	
	if len(args) > 2 {
		timestamp, err = strconv.ParseInt(args[2], 10, 64)
		if (err != nil) {
			http.Error(w, "Error getting speed from Play request.", http.StatusBadRequest)
			return
		}
	}
	
	var ret string
	if (flight == 0) {
		if (!globalStatus.ReplayMode) {
			http.Error(w, "Cannot cancel replay - no replay active.", http.StatusBadRequest)
			return
		} else {
			abortReplay = true
		}
	} else {
		abortReplay = false
		go replayFlightLog(flight, speed, timestamp)
		ret = fmt.Sprintf("{\"status\": \"playing\", \"speed\": %d, \"flight\": %d, \"timestamp\": %d}", speed, flight, timestamp)
	}
	
	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "%s\n", ret)

}

func handleFlightLogReplayPause(args []string, w http.ResponseWriter, r *http.Request) {

	if (globalStatus.ReplayMode == false) {
		http.Error(w, "Cannot pause replay - no replay active.", http.StatusBadRequest)
		return
	}
	
	pauseReplay = true
	
	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "{\"status\": \"paused\"}\n")
	
}

func handleFlightLogReplayResume(args []string, w http.ResponseWriter, r *http.Request) {

	if (globalStatus.ReplayMode == false) {
		http.Error(w, "Cannot pause replay - no replay active.", http.StatusBadRequest)
		return
	}
	
	pauseReplay = false
	
	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "{\"status\": \"playing\"}\n")
	
}

func handleFlightLogReplaySpeed(args []string, w http.ResponseWriter, r *http.Request) {
	
	if (globalStatus.ReplayMode == false) {
		http.Error(w, "Cannot pause replay - no replay active.", http.StatusBadRequest)
		return
	}
	
	if len(args) < 1 {
		http.Error(w, "Error getting speed from Speed request.", http.StatusBadRequest)
		return
	}
		
	speed, err := strconv.ParseInt(args[0], 10, 64)
	if (err != nil) {
		http.Error(w, "Error getting speed from Play request.", http.StatusBadRequest)
		return
	}

	replaySpeed = speed
	replayStatus.Speed = speed
	
	fmt.Printf("Setting replay speed to %d\n", replaySpeed);
	
	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "{\"status\": \"playing\", \"speed\": %d}\n", speed)
	
}

func handleFlightLogReplayStop(args []string, w http.ResponseWriter, r *http.Request) {

	if (globalStatus.ReplayMode == false) {
		http.Error(w, "Cannot cancel replay - no replay active.", http.StatusBadRequest)
		return
	}
	
	abortReplay = true
	
	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "{\"status\": \"stopping\"}\n")

}

func handleFlightLogReplayJump(args []string, w http.ResponseWriter, r *http.Request) {

	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "{\"status\": \"jumping\"}\n")
	
}

func handleFlightLogReplayStatus(args []string, w http.ResponseWriter, r *http.Request) {

	setNoCache(w)
	setJSONHeaders(w)
	fmt.Fprintf(w, "{\"status\": \"happy!\"}\n")
	
}


func handleReplayRequest(w http.ResponseWriter, r *http.Request) {
		
	// /replay/play/12/5/1 (replay flight 12 on a loop)
	// /replay/pause (stop at current timestamp - returns current timestamp)
	// /replay/resume (resume playing after pause)
	// /replay/speed/3 (adjust the playback speed)
	// /replay/stop (cancel current playback)
	// /replay/jump/392952 (jump to timestamp 392952 and play)
	// /replay/status (returns the current status and, if playing, timestamp)
	
	path := strings.Split(r.URL.String(), "/")
	
	// minimum of 3 elements
	if len(path) < 3 {
		http.Error(w, "Replay requests require a command.", http.StatusBadRequest)
		return
	}
	
	// everything starts with "/replay"
	if path[1] != "replay" {
		http.Error(w, "Error - missing 'replay' prefix.", http.StatusBadRequest)
		return
	}
	
	command := path[2]
	arguments := path[3:]
	
	switch command {
	case "play":
		handleFlightLogReplayPlay(arguments, w, r)
	case "pause":
		handleFlightLogReplayPause(arguments, w, r)
	case "resume":
		handleFlightLogReplayResume(arguments, w, r)
	case "speed":
		handleFlightLogReplaySpeed(arguments, w, r)
	case "stop":
		handleFlightLogReplayStop(arguments, w, r)
	case "jump":
		handleFlightLogReplayJump(arguments, w, r)
	case "status":
		handleFlightLogReplayStatus(arguments, w, r)
	default:
		http.Error(w, "Error - invalid FlightLog command.", http.StatusBadRequest)
	}
}

func delayReboot() {
	time.Sleep(1 * time.Second)
	doReboot()
}

// Upload an update file.
func handleUpdatePostRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	r.ParseMultipartForm(1024 * 1024 * 32) // ~32MB update.
	file, handler, err := r.FormFile("update_file")
	if err != nil {
		log.Printf("Update failed from %s (%s).\n", r.RemoteAddr, err.Error())
		return
	}
	defer file.Close()
	// Special hardware builds. Don't allow an update unless the filename contains the hardware build name.
	if (len(globalStatus.HardwareBuild) > 0) && !strings.Contains(strings.ToLower(handler.Filename), strings.ToLower(globalStatus.HardwareBuild)) {
		w.WriteHeader(404)
		return
	}
	updateFile := fmt.Sprintf("/root/update-stratux-v.sh")
	f, err := os.OpenFile(updateFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		log.Printf("Update failed from %s (%s).\n", r.RemoteAddr, err.Error())
		return
	}
	defer f.Close()
	io.Copy(f, file)
	log.Printf("%s uploaded %s for update.\n", r.RemoteAddr, updateFile)
	// Successful update upload. Now reboot.
	go delayReboot()
}

func setNoCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func setJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
}

func defaultServer(w http.ResponseWriter, r *http.Request) {
	//	setNoCache(w)

	http.FileServer(http.Dir("/var/www")).ServeHTTP(w, r)
}

func handleroPartitionRebuild(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("/usr/sbin/rebuild_ro_part.sh").Output()

	var ret_err error
	if err != nil {
		ret_err = fmt.Errorf("Rebuild RO Partition error: %s", err.Error())
	} else {
		ret_err = fmt.Errorf("Rebuild RO Partition success: %s", out)
	}

	addSystemError(ret_err)
}

// https://gist.github.com/alexisrobert/982674.
// Copyright (c) 2010-2014 Alexis ROBERT <alexis.robert@gmail.com>.
const dirlisting_tpl = `<?xml version="1.0" encoding="iso-8859-1"?>
<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.1//EN" "http://www.w3.org/TR/xhtml11/DTD/xhtml11.dtd">
<html xmlns="http://www.w3.org/1999/xhtml" xml:lang="en">
<!-- Modified from lighttpd directory listing -->
<head>
<title>Index of {{.Name}}</title>
<style type="text/css">
a, a:active {text-decoration: none; color: blue;}
a:visited {color: #48468F;}
a:hover, a:focus {text-decoration: underline; color: red;}
body {background-color: #F5F5F5;}
h2 {margin-bottom: 12px;}
table {margin-left: 12px;}
th, td { font: 90% monospace; text-align: left;}
th { font-weight: bold; padding-right: 14px; padding-bottom: 3px;}
td {padding-right: 14px;}
td.s, th.s {text-align: right;}
div.list { background-color: white; border-top: 1px solid #646464; border-bottom: 1px solid #646464; padding-top: 10px; padding-bottom: 14px;}
div.foot { font: 90% monospace; color: #787878; padding-top: 4px;}
</style>
</head>
<body>
<h2>Index of {{.Name}}</h2>
<div class="list">
<table summary="Directory Listing" cellpadding="0" cellspacing="0">
<thead><tr><th class="n">Name</th><th>Last Modified</th><th>Size (bytes)</th><th class="dl">Options</th></tr></thead>
<tbody>
{{range .Children_files}}
<tr><td class="n"><a href="/logs/stratux/{{.Name}}">{{.Name}}</a></td><td>{{.Mtime}}</td><td>{{.Size}}</td><td class="dl"><a href="/logs/stratux/{{.Name}}">Download</a></td></tr>
{{end}}
</tbody>
</table>
</div>
<div class="foot">{{.ServerUA}}</div>
</body>
</html>`

type fileInfo struct {
	Name  string
	Mtime string
	Size  string
}

// Manages directory listings
type dirlisting struct {
	Name           string
	Children_files []fileInfo
	ServerUA       string
}

//FIXME: This needs to be switched to show a "sessions log" from the sqlite database.
func viewLogs(w http.ResponseWriter, r *http.Request) {

	var logPath string
	
	if _, err := os.Stat("/etc/FlightBox"); !os.IsNotExist(err) {
		logPath = "/root/log/"
	} else { // if not using the FlightBox config, use "normal" log file locations
		logPath = "/var/log/stratux/"
	}
	
	names, err := ioutil.ReadDir(logPath)
	if err != nil {
		return
	}

	fi := make([]fileInfo, 0)
	for _, val := range names {
		if val.Name()[0] == '.' {
			continue
		} // Remove hidden files from listing

		if !val.IsDir() {
			mtime := val.ModTime().Format("2006-Jan-02 15:04:05")
			sz := humanize.Comma(val.Size())
			fi = append(fi, fileInfo{Name: val.Name(), Mtime: mtime, Size: sz})
		}
	}

	tpl, err := template.New("tpl").Parse(dirlisting_tpl)
	if err != nil {
		return
	}
	data := dirlisting{Name: r.URL.Path, ServerUA: "Stratux " + stratuxVersion + "/" + stratuxBuild,
		Children_files: fi}

	err = tpl.Execute(w, data)
	if err != nil {
		log.Printf("viewLogs() error: %s\n", err.Error())
	}

}

func managementInterface() {
	weatherUpdate = NewUIBroadcaster()
	trafficUpdate = NewUIBroadcaster()

	http.HandleFunc("/", defaultServer)
	
	var logPath string
	if _, err := os.Stat("/etc/FlightBox"); !os.IsNotExist(err) {
		logPath = "/root/log"
	} else { // if not using the FlightBox config, use "normal" log file locations
		logPath = "/var/log"
	}
	http.Handle("/logs/", http.StripPrefix("/logs/", http.FileServer(http.Dir(logPath))))
	http.Handle("/logs/stratux/", http.StripPrefix("/logs/stratux/", http.FileServer(http.Dir(logPath))))
	http.HandleFunc("/view_logs/", viewLogs)

	http.HandleFunc("/status",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleStatusWS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/situation",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleSituationWS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/weather",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleWeatherWS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/traffic",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleTrafficWS)}
			s.ServeHTTP(w, req)
		})
	http.HandleFunc("/replay/socket",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleReplayWS)}
			s.ServeHTTP(w, req)
		})

	http.HandleFunc("/getStatus", handleStatusRequest)
	http.HandleFunc("/getSituation", handleSituationRequest)
	http.HandleFunc("/getTowers", handleTowersRequest)
	http.HandleFunc("/getSatellites", handleSatellitesRequest)
	http.HandleFunc("/getSettings", handleSettingsGetRequest)
	http.HandleFunc("/setSettings", handleSettingsSetRequest)
	http.HandleFunc("/shutdown", handleShutdownRequest)
	http.HandleFunc("/reboot", handleRebootRequest)
	http.HandleFunc("/getClients", handleClientsGetRequest)
	http.HandleFunc("/updateUpload", handleUpdatePostRequest)
	http.HandleFunc("/roPartitionRebuild", handleroPartitionRebuild)
	http.HandleFunc("/flightlog/", handleFlightLogRequest)
	http.HandleFunc("/replay/", handleReplayRequest)
	
	err := http.ListenAndServe(managementAddr, nil)

	if err != nil {
		log.Printf("managementInterface ListenAndServe: %s\n", err.Error())
	}
}
