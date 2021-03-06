/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New"" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	datalog.go: Log stratux data as it is received. Bucket data into timestamp time slots.

*/

package main

import (
	"database/sql"
	"errors"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
	"encoding/json"
	"github.com/kellydunn/golang-geo"
	"github.com/bradfitz/latlong"
)

const (
	LOG_TIMESTAMP_RESOLUTION = 250 * time.Millisecond
	FLIGHT_SPEED = 55
	MIN_FLIGHT_SPEED = 45
	TAXI_SPEED = 5
	MIN_TAXI_SPEED = 0
	NM_PER_KM = 0.539957
	
	FLIGHT_STATE_UNKNOWN = -1
	FLIGHT_STATE_STOPPED = 0
	FLIGHT_STATE_TAXIING = 1
	FLIGHT_STATE_FLYING = 2
)

type StratuxTimestamp struct {
	id                   int64
	Time_type_preference int // 0 = stratuxClock, 1 = gpsClock, 2 = gpsClock extrapolated via stratuxClock.
	StratuxClock_value   time.Time
	GPSClock_value       time.Time // The value of this is either from the GPS or extrapolated from the GPS via stratuxClock if pref is 1 or 2. It is time.Time{} if 0.
	PreferredTime_value  time.Time
	StartupID            int64
}

type ReplayData struct {
	Flight int64
	Timestamp int64
	Speed int64
}

var replayStatus ReplayData

var dataLogStarted bool
var dataLogReadyToWrite bool
var lastSituationLogMs uint64

var stratuxStartupID int64
var dataLogTimestamps []StratuxTimestamp
var dataLogCurTimestamp int64 // Current timestamp bucket. This is an index on dataLogTimestamps which is not necessarily the db id.

/*
	values / flags used by flight logging code (see: logSituation() below)
*/
var lastPoint *geo.Point

//TODO: Make this a user-configurable option, either manually or using aircraft profile
var startTaxiingSpeed uint16 = TAXI_SPEED
var stopTaxiingSpeed uint16 = MIN_TAXI_SPEED
var startFlyingSpeed uint16 = FLIGHT_SPEED
var stopFlyingSpeed uint16 = MIN_FLIGHT_SPEED

var flightState0 int = FLIGHT_STATE_UNKNOWN
var flightState1 int = FLIGHT_STATE_UNKNOWN
var flightState2 int = FLIGHT_STATE_UNKNOWN
/*
	airport structure - used by the airport lookup utility
*/
type airport struct {
	faaId string
	icaoId string
	name string
	lat float64
	lng float64
	alt float64
	dst float64
}

type FlightEvent struct {
	id int64
	event string
	lat float64
	lng float64
	localtime string
	airport_id string
	airport_name string
	timestamp int64
}
/*
	checkTimestamp().
		Verify that our current timestamp is within the LOG_TIMESTAMP_RESOLUTION bucket.
		 Returns false if the timestamp was changed, true if it is still valid.
		 This is where GPS timestamps are extrapolated, if the GPS data is currently valid.
*/

func checkTimestamp() bool {
	thisCurTimestamp := dataLogCurTimestamp
	if stratuxClock.Since(dataLogTimestamps[thisCurTimestamp].StratuxClock_value) >= LOG_TIMESTAMP_RESOLUTION {
		var ts StratuxTimestamp
		ts.id = 0
		ts.Time_type_preference = 0 // stratuxClock.
		ts.StratuxClock_value = stratuxClock.Time
		ts.GPSClock_value = time.Time{}
		ts.PreferredTime_value = stratuxClock.Time

		// Extrapolate from GPS timestamp, if possible.
		if isGPSClockValid() && thisCurTimestamp > 0 {
			// Was the last timestamp either extrapolated or GPS time?
			last_ts := dataLogTimestamps[thisCurTimestamp]
			if last_ts.Time_type_preference == 1 || last_ts.Time_type_preference == 2 {
				// Extrapolate via stratuxClock.
				timeSinceLastTS := ts.StratuxClock_value.Sub(last_ts.StratuxClock_value) // stratuxClock ticks since last timestamp.
				extrapolatedGPSTimestamp := last_ts.PreferredTime_value.Add(timeSinceLastTS)

				// Re-set the preferred timestamp type to '2' (extrapolated time).
				ts.Time_type_preference = 2
				ts.PreferredTime_value = extrapolatedGPSTimestamp
				ts.GPSClock_value = extrapolatedGPSTimestamp
			}
		}

		dataLogTimestamps = append(dataLogTimestamps, ts)
		dataLogCurTimestamp = int64(len(dataLogTimestamps) - 1)
		return false
	}
	return true
}

type SQLiteMarshal struct {
	FieldType string
	Marshal   func(v reflect.Value) string
}

func boolMarshal(v reflect.Value) string {
	b := v.Bool()
	if b {
		return "1"
	}
	return "0"
}

func intMarshal(v reflect.Value) string {
	return strconv.FormatInt(v.Int(), 10)
}

func uintMarshal(v reflect.Value) string {
	return strconv.FormatUint(v.Uint(), 10)
}

func floatMarshal(v reflect.Value) string {
	return strconv.FormatFloat(v.Float(), 'f', 10, 64)
}

func stringMarshal(v reflect.Value) string {
	return v.String()
}

func notsupportedMarshal(v reflect.Value) string {
	return ""
}

func structCanBeMarshalled(v reflect.Value) bool {
	m := v.MethodByName("String")
	if m.IsValid() && !m.IsNil() {
		return true
	}
	return false
}

func structMarshal(v reflect.Value) string {
	if structCanBeMarshalled(v) {
		m := v.MethodByName("String")
		in := make([]reflect.Value, 0)
		ret := m.Call(in)
		if len(ret) > 0 {
			return ret[0].String()
		}
	}
	return ""
}

var sqliteMarshalFunctions = map[string]SQLiteMarshal{
	"bool":         {FieldType: "INTEGER", Marshal: boolMarshal},
	"int":          {FieldType: "INTEGER", Marshal: intMarshal},
	"uint":         {FieldType: "INTEGER", Marshal: uintMarshal},
	"float":        {FieldType: "REAL", Marshal: floatMarshal},
	"string":       {FieldType: "TEXT", Marshal: stringMarshal},
	"struct":       {FieldType: "STRING", Marshal: structMarshal},
	"notsupported": {FieldType: "notsupported", Marshal: notsupportedMarshal},
}

var sqlTypeMap = map[reflect.Kind]string{
	reflect.Bool:          "bool",
	reflect.Int:           "int",
	reflect.Int8:          "int",
	reflect.Int16:         "int",
	reflect.Int32:         "int",
	reflect.Int64:         "int",
	reflect.Uint:          "uint",
	reflect.Uint8:         "uint",
	reflect.Uint16:        "uint",
	reflect.Uint32:        "uint",
	reflect.Uint64:        "uint",
	reflect.Uintptr:       "notsupported",
	reflect.Float32:       "float",
	reflect.Float64:       "float",
	reflect.Complex64:     "notsupported",
	reflect.Complex128:    "notsupported",
	reflect.Array:         "notsupported",
	reflect.Chan:          "notsupported",
	reflect.Func:          "notsupported",
	reflect.Interface:     "notsupported",
	reflect.Map:           "notsupported",
	reflect.Ptr:           "notsupported",
	reflect.Slice:         "notsupported",
	reflect.String:        "string",
	reflect.Struct:        "struct",
	reflect.UnsafePointer: "notsupported",
}

func makeTable(i interface{}, tbl string, db *sql.DB) {
	val := reflect.ValueOf(i)

	fields := make([]string, 0)
	for i := 0; i < val.NumField(); i++ {
		kind := val.Field(i).Kind()
		fieldName := val.Type().Field(i).Name
		sqlTypeAlias := sqlTypeMap[kind]

		// Check that if the field is a struct that it can be marshalled.
		if sqlTypeAlias == "struct" && !structCanBeMarshalled(val.Field(i)) {
			continue
		}
		if sqlTypeAlias == "notsupported" || fieldName == "id" {
			continue
		}
		sqlType := sqliteMarshalFunctions[sqlTypeAlias].FieldType
		s := fieldName + " " + sqlType
		fields = append(fields, s)
	}

	// Add the timestamp_id field to link up with the timestamp table.
	if tbl != "timestamp" && tbl != "startup" {
		fields = append(fields, "timestamp_id INTEGER")
		fields = append(fields, "startup_id INTEGER")
	}
	
	tblCreate := fmt.Sprintf("CREATE TABLE %s (id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, %s)", tbl, strings.Join(fields, ", "))

	_, err := db.Exec(tblCreate)
	if err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
	}
}

/*
	bulkInsert().
		Reads insertBatch and insertBatchIfs. This is called after a group of insertData() calls.
*/

func bulkInsert(tbl string, db *sql.DB) (res sql.Result, err error) {
	if _, ok := insertString[tbl]; !ok {
		return nil, errors.New("no insert statement")
	}

	batchVals := insertBatchIfs[tbl]
	numColsPerRow := len(batchVals[0])
	maxRowBatch := int(999 / numColsPerRow) // SQLITE_MAX_VARIABLE_NUMBER = 999.
	//	log.Printf("table %s. %d cols per row. max batch %d\n", tbl, numColsPerRow, maxRowBatch)
	for len(batchVals) > 0 {
		//     timeInit := time.Now()
		i := int(0) // Variable number of rows per INSERT statement.

		stmt := ""
		vals := make([]interface{}, 0)
		querySize := uint64(0)                                            // Size of the query in bytes.
		for len(batchVals) > 0 && i < maxRowBatch && querySize < 750000 { // Maximum of 1,000,000 bytes per query.
			if len(stmt) == 0 { // The first set will be covered by insertString.
				stmt = insertString[tbl]
				querySize += uint64(len(insertString[tbl]))
			} else {
				addStr := ", (" + strings.Join(strings.Split(strings.Repeat("?", len(batchVals[0])), ""), ",") + ")"
				stmt += addStr
				querySize += uint64(len(addStr))
			}
			for _, val := range batchVals[0] {
				querySize += uint64(len(val.(string)))
			}
			vals = append(vals, batchVals[0]...)
			batchVals = batchVals[1:]
			i++
		}
		//		log.Printf("inserting %d rows to %s. querySize=%d\n", i, tbl, querySize)
		res, err = db.Exec(stmt, vals...)
		//      timeBatch := time.Since(timeInit)                                                                                                                     // debug
		//      log.Printf("SQLite: bulkInserted %d rows to %s. Took %f msec to build and insert query. querySize=%d\n", i, tbl, 1000*timeBatch.Seconds(), querySize) // debug
		if err != nil {
			log.Printf("sqlite INSERT error: '%s'\n", err.Error())
			return
		}
	}

	// Clear the buffers.
	delete(insertString, tbl)
	delete(insertBatchIfs, tbl)

	return
}

/*
	insertData().
		Inserts an arbitrary struct into an SQLite table.
		 Inserts the timestamp first, if its 'id' is 0.

*/

// Cached 'VALUES' statements. Indexed by table name.
var insertString map[string]string // INSERT INTO tbl (col1, col2, ...) VALUES(?, ?, ...). Only for one value.
var insertBatchIfs map[string][][]interface{}

func insertData(i interface{}, tbl string, db *sql.DB, ts_num int64) int64 {
	val := reflect.ValueOf(i)

	keys := make([]string, 0)
	values := make([]string, 0)
	for i := 0; i < val.NumField(); i++ {
		kind := val.Field(i).Kind()
		fieldName := val.Type().Field(i).Name
		sqlTypeAlias := sqlTypeMap[kind]

		if sqlTypeAlias == "notsupported" || fieldName == "id" {
			continue
		}

		v := sqliteMarshalFunctions[sqlTypeAlias].Marshal(val.Field(i))

		keys = append(keys, fieldName)
		values = append(values, v)
	}

	// Add the timestamp_id and startup_id fields
	if tbl != "timestamp" && tbl != "startup" {
		keys = append(keys, "timestamp_id")
		values = append(values, strconv.FormatInt(int64(stratuxClock.Milliseconds), 10))
		keys = append(keys, "startup_id")
		values = append(values, strconv.FormatInt(stratuxStartupID, 10))
	}

	if _, ok := insertString[tbl]; !ok {
		// Prepare the statement.
		tblInsert := fmt.Sprintf("INSERT INTO %s (%s) VALUES(%s)", tbl, strings.Join(keys, ","),
			strings.Join(strings.Split(strings.Repeat("?", len(keys)), ""), ","))
		insertString[tbl] = tblInsert
	}

	// Make the values slice into a slice of interface{}.
	ifs := make([]interface{}, len(values))
	for i := 0; i < len(values); i++ {
		ifs[i] = values[i]
	}

	insertBatchIfs[tbl] = append(insertBatchIfs[tbl], ifs)

	if tbl == "timestamp" || tbl == "startup" { // Immediate insert always for "timestamp" and "startup" table.
		res, err := bulkInsert(tbl, db) // Bulk insert of 1, always.
		if err == nil {
			id, err := res.LastInsertId()
			if err == nil && tbl == "timestamp" { // Special handling for timestamps. Update the timestamp ID.
				ts := dataLogTimestamps[ts_num]
				ts.id = id
				dataLogTimestamps[ts_num] = ts
			}
			return id
		}
	}

	return 0
}

type DataLogRow struct {
	tbl    string
	data   interface{}
	ts_num int64
}

var dataLogChan chan DataLogRow
var shutdownDataLog chan bool
var shutdownDataLogWriter chan bool
var dataUpdateChan chan bool
var dataLogWriteChan chan DataLogRow
var replayChan chan ReplayData

func dataLogWriter(db *sql.DB) {
	dataLogWriteChan = make(chan DataLogRow, 10240)
	shutdownDataLogWriter = make(chan bool)
	dataUpdateChan = make(chan bool, 1024)
	// The write queue. As data comes in via dataLogChan, it is timestamped and stored.
	//  When writeTicker comes up, the queue is emptied.
	writeTicker := time.NewTicker(1 * time.Second)
	rowsQueuedForWrite := make([]DataLogRow, 0)
	for {
		select {
		case r := <-dataLogWriteChan:
			// Accept timestamped row.
			rowsQueuedForWrite = append(rowsQueuedForWrite, r)
		case <-dataUpdateChan:
			// Start transaction.
			tx, err := db.Begin()
			if err != nil {
				log.Printf("db.Begin() error: %s\n", err.Error())
				break // from select {}
			}
			updateFlightLog(db)
			// Close the transaction.
			tx.Commit()
		case <-writeTicker.C:
			//			for i := 0; i < 1000; i++ {
			//				logSituation()
			//			}
			timeStart := stratuxClock.Time
			nRows := len(rowsQueuedForWrite)
			if globalSettings.DEBUG {
				log.Printf("Writing %d rows\n", nRows)
			}
			// Write the buffered rows. This will block while it is writing.
			// Save the names of the tables affected so that we can run bulkInsert() on after the insertData() calls.
			tblsAffected := make(map[string]bool)
			// Start transaction.
			tx, err := db.Begin()
			if err != nil {
				log.Printf("db.Begin() error: %s\n", err.Error())
				break // from select {}
			}
			for _, r := range rowsQueuedForWrite {
				tblsAffected[r.tbl] = true
				insertData(r.data, r.tbl, db, r.ts_num)
			}
			// Do the bulk inserts.
			for tbl, _ := range tblsAffected {
				bulkInsert(tbl, db)
			}
			// Close the transaction.
			tx.Commit()
			rowsQueuedForWrite = make([]DataLogRow, 0) // Zero the queue.
			timeElapsed := stratuxClock.Since(timeStart)
			if globalSettings.DEBUG {
				rowsPerSecond := float64(nRows) / float64(timeElapsed.Seconds())
				log.Printf("Writing finished. %d rows in %.2f seconds (%.1f rows per second).\n", nRows, float64(timeElapsed.Seconds()), rowsPerSecond)
			}
			if timeElapsed.Seconds() > 10.0 {
				log.Printf("WARNING! SQLite logging is behind. Last write took %.1f seconds.\n", float64(timeElapsed.Seconds()))
				dataLogCriticalErr := fmt.Errorf("WARNING! SQLite logging is behind. Last write took %.1f seconds.\n", float64(timeElapsed.Seconds()))
				addSystemError(dataLogCriticalErr)
			}
		case <-shutdownDataLogWriter: // Received a message on the channel to initiate a graceful shutdown, and to command dataLog() to shut down
			log.Printf("datalog.go: dataLogWriter() received shutdown message with rowsQueuedForWrite = %d\n", len(rowsQueuedForWrite))
			shutdownDataLog <- true
			return
		}
	}
	log.Printf("datalog.go: dataLogWriter() shutting down\n")
}

func dataLog() {
	dataLogStarted = true
	log.Printf("datalog.go: dataLog() started\n")
	dataLogChan = make(chan DataLogRow, 10240)
	shutdownDataLog = make(chan bool)
	dataLogTimestamps = make([]StratuxTimestamp, 0)
	var ts StratuxTimestamp
	ts.id = 0
	ts.Time_type_preference = 0 // stratuxClock.
	ts.StratuxClock_value = stratuxClock.Time
	ts.GPSClock_value = time.Time{}
	ts.PreferredTime_value = stratuxClock.Time
	dataLogTimestamps = append(dataLogTimestamps, ts)
	dataLogCurTimestamp = 0

	// Check if we need to create a new database.
	createDatabase := false

	if _, err := os.Stat(dataLogFilef); os.IsNotExist(err) {
		createDatabase = true
		log.Printf("creating new database '%s'.\n", dataLogFilef)
	}

	db, err := sql.Open("sqlite3", dataLogFilef)
	if err != nil {
		log.Printf("sql.Open(): %s\n", err.Error())
	}

	defer func() {
		db.Close()
		dataLogStarted = false
		//close(dataLogChan)
		log.Printf("datalog.go: dataLog() has closed DB in %s\n", dataLogFilef)
	}()

	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		log.Printf("db.Exec('PRAGMA journal_mode=WAL') err: %s\n", err.Error())
	}
	_, err = db.Exec("PRAGMA synchronous=OFF")
	if err != nil {
		log.Printf("db.Exec('PRAGMA journal_mode=WAL') err: %s\n", err.Error())
	}

	//log.Printf("Starting dataLogWriter\n") // REMOVE -- DEBUG
	go dataLogWriter(db)

	// Do we need to create the database?
	if createDatabase {
		makeTable(StratuxTimestamp{}, "timestamp", db)
		makeTable(mySituation, "mySituation", db)
		makeTable(globalStatus, "status", db)
		makeTable(globalSettings, "settings", db)
		makeTable(TrafficInfo{}, "traffic", db)
		makeTable(msg{}, "messages", db)
		makeTable(esmsg{}, "es_messages", db)
		makeTable(Dump1090TermMessage{}, "dump1090_terminal", db)
		makeTable(FlightLog{}, "startup", db)
		makeTable(FlightEvent{}, "events", db)
	}

	// The first entry to be created is the "startup" entry.
	stratuxStartupID = insertData(FlightLog{}, "startup", db, 0)

	dataLogReadyToWrite = true
	//log.Printf("Entering dataLog read loop\n") //REMOVE -- DEBUG
	for {
		select {
		case r := <-dataLogChan:
			// When data is input, the first step is to timestamp it.
			// Check if our time bucket has expired or has never been entered.
			checkTimestamp()
			// Mark the row with the current timestamp ID, in case it gets entered later.
			r.ts_num = dataLogCurTimestamp
			// Queue it for the scheduled write.
			dataLogWriteChan <- r
		case <-shutdownDataLog: // Received a message on the channel to complete a graceful shutdown (see the 'defer func()...' statement above).
			log.Printf("datalog.go: dataLog() received shutdown message\n")
			return
		}
	}
	log.Printf("datalog.go: dataLog() shutting down\n")
	close(shutdownDataLog)
}

/*
	setDataLogTimeWithGPS().
		Create a timestamp entry using GPS time.
*/

func setDataLogTimeWithGPS(sit SituationData) {
	/* 
		TODO: we only need to run this function once to set the start time value
		in the current startup record. Calculate the number of milliseconds since the
		system started logging data (stratuxClock init value) and now. Convert the 
		GPS time into milliseconds, then subtract the difference. That gives you the
		start time (UTC) in milliseconds. (In other words, Unix Timestamp in Milliseconds.)
		
		Take that value and stick it in the current "startup" record. 
	
	*/
	if isGPSClockValid() {
		var ts StratuxTimestamp
		// Piggyback a GPS time update from this update.
		ts.id = 0
		ts.Time_type_preference = 1 // gpsClock.
		ts.StratuxClock_value = stratuxClock.Time
		ts.GPSClock_value = sit.GPSTime
		ts.PreferredTime_value = sit.GPSTime

		dataLogTimestamps = append(dataLogTimestamps, ts)
		dataLogCurTimestamp = int64(len(dataLogTimestamps) - 1)
	}
}

/*
	logSituation(), logStatus(), ... pass messages from other functions to the logging
		engine. These are only read into `dataLogChan` if the Replay Log is toggled on,
		and if the log system is ready to accept writes.
*/

func isDataLogReady() bool {
	return dataLogReadyToWrite
}

/*
	findAirport(): a simple, quick process for locating the nearest airport to a given
	set of coordinates. In this case the function is limited to searching within 0.1
	degrees of the input coordinates.
	
	Note: expects to find the "airports.sqlite" file in /root/log
	
	The database is compiled from the FAAs NACAR 56-day subscription database and
	includes all airports including private and heliports.
	
	TODO: Find a source for ALL airports
*/
func findAirport(lat float64, lng float64) (airport, error) {
	
	// return value
	var ret airport

	aptdb, err := sql.Open("sqlite3", "/root/log/airports.sqlite")
	if err != nil {
		return ret, err
	}
	
	defer aptdb.Close()
	
	minLat := lat - 0.1
	minLng := lng - 0.1
	maxLat := lat + 0.1
	maxLng := lng + 0.1
	
	p := geo.NewPoint(lat, lng)
	
	// TODO: return an ICAO ID if there is no FAA ID, or perhaps the other way around
	rows, err := aptdb.Query("SELECT faaid, icaoid, name, lat, lng, alt FROM airport WHERE lat > ? AND lat < ? AND lng > ? AND lng < ? ORDER BY id ASC;", minLat, maxLat, minLng, maxLng)
	if err != nil {
		return ret, err
	}
	
	for rows.Next() {
		var r airport
		err = rows.Scan(&r.faaId, &r.icaoId, &r.name, &r.lat, &r.lng, &r.alt)
		ap := geo.NewPoint(r.lat, r.lng)
		r.dst = ap.GreatCircleDistance(p)
		
		if (ret.faaId == "") {
			ret = r
		} else if (r.dst < ret.dst) {
			ret = r
		}
	}
	
	return ret, nil
}

/*
	FlightLog structure - replaces 'startup' structure as the basis for the startup
	table in the SQLite database. A single FlightLog variable is used throughout a
	session (startup) to track flight log information.
*/
type FlightLog struct {
	id int64
	start_airport_id string
	start_airport_name string
	start_timestamp int64
	start_localtime string
	start_tz string
	start_lat float64
	start_lng float64
	start_alt float32
	
	end_airport_id string
	end_airport_name string
	end_timestamp int64
	end_localtime string
	end_tz string
	end_lat float64
	end_lng float64
	
	max_alt float32
	duration int64
	distance float64
	groundspeed int64
	
	route string
}

var flightlog FlightLog

/*
	replayFlightLog(flight int): replay a flight at a given speed
*/

var replaySpeed int64 = 1
var pauseReplay bool
var abortReplay bool
var uatReplayComplete bool
var esReplayComplete bool
var situationReplayComplete bool

func resetReplay() {
	globalStatus.ReplayMode = false
	replayStatus.Flight = 0
	replayStatus.Speed = 0
	replayStatus.Timestamp = 0
}

func replayUAT(flight int64, db *sql.DB, timestamp int64) {
	
	var ts1, ts2 int64
	var data string
	var msgCount int64
	
	uatReplayComplete = false
	
	sql := fmt.Sprintf("SELECT timestamp_id, data FROM messages WHERE startup_id = %d AND timestamp_id > %d ORDER BY timestamp_id ASC;", flight, timestamp)
	rows, err := db.Query(sql)
	if err != nil {
		fmt.Printf("Error querying messages: %s\n", err.Error())
		return
	}
	
	defer rows.Close()
	
	for rows.Next() {
		
		msgCount++
		
		if (ts1 == 0) {
			err = rows.Scan(&ts1, &data)
			if (err != nil) {
				fmt.Printf("Error scanning row 1: %s\n", err.Error())
				uatReplayComplete = true
				return
			}
			continue
		}
		
		if (ts2 == 0) {
			err = rows.Scan(&ts2, &data)
			if (err != nil) {
				fmt.Printf("Error scanning row 2: %s\n", err.Error())
				uatReplayComplete = true
				return
			}
		} 

		if data == "" {
			continue
		}
		
		// wait for the appropriate number of ms
		var counter int64 = 0
		delta := (ts2 - ts1)
		wait := (delta / replaySpeed)
		
		// drop messages inversely proportional to speed of playback (i.e. 0 drop at 1x, 90% drop at 10x)
		if (msgCount % replaySpeed) == 0 {
			
			for {
				time.Sleep(1 * time.Millisecond)
				counter++
				if abortReplay || (counter >= wait) {
					break;
				}
			}
			
			// queue the message
			o, msgtype := parseInput(data)
			if o != nil && msgtype != 0 {
				relayMessage(msgtype, o)
			}	
		}
		
		// shuffle the timestamps
		ts1 = ts2
		ts2 = 0

		if pauseReplay {
			for {
				if (!pauseReplay) || (abortReplay) {
					break
				}
				time.Sleep(1 * time.Millisecond)
			}
		}
		
		if abortReplay {
			uatReplayComplete = true
			break
		}
	}
	

	uatReplayComplete = true
	if uatReplayComplete && esReplayComplete && situationReplayComplete {
		resetReplay()
	}
}

func replay1090(flight int64, db *sql.DB, timestamp int64) {
	
	var ts1, ts2 int64
	var data string
	var msgCount int64
	
	esReplayComplete = false
	
	sql := fmt.Sprintf("SELECT timestamp_id, data FROM es_messages WHERE startup_id = %d AND timestamp_id > %d ORDER BY timestamp_id ASC;", flight, timestamp)
	rows, err := db.Query(sql)
	if err != nil {
		return
	}
	
	defer rows.Close()
	
	for rows.Next() {
		
		if (ts1 == 0) {
			err = rows.Scan(&ts1, &data)
			if (err != nil) {
				fmt.Printf("Error scanning row 1: %s\n", err.Error())
				esReplayComplete = true
				return
			}
			continue
		}
		
		if (ts2 == 0) {
			err = rows.Scan(&ts2, &data)
			if (err != nil) {
				fmt.Printf("Error scanning row 2: %s\n", err.Error())
				esReplayComplete = true
				return
			}
		} 
		
		// wait for the appropriate number of ms
		var counter int64 = 0
		delta := (ts2 - ts1)
		wait := (delta / replaySpeed)
		
		// drop messages inversely proportional to speed
		if (msgCount % replaySpeed) == 0 {
			
			for {
				time.Sleep(1 * time.Millisecond)
				counter++
				if abortReplay || (counter >= wait) {
					break;
				}
			}
			
			// queue the 1090-ES message
			var newTi *dump1090Data
			err = json.Unmarshal([]byte(data), &newTi)
			if err != nil {
				log.Printf("can't read ES traffic information from %s: %s\n", data, err.Error())
			} else {
				parseDump1090Record(newTi)
			}
			
		}
		
		// shuffle the timestamps
		ts1 = ts2
		ts2 = 0
		
		if pauseReplay {
			for {
				if (!pauseReplay) || (abortReplay) {
					break
				}
				time.Sleep(1 * time.Millisecond)
			}
		}

		if abortReplay {
			esReplayComplete = true
			break
		}
	}
	
	esReplayComplete = true
	if uatReplayComplete && esReplayComplete && situationReplayComplete {
		resetReplay()
	} 
	
}

/*
	Rather than trying to reload a complete mySituation structure from the database
	(which is painful due to the lack of discrete date types in SQLite), we're just
	going to pull the stuff we need to create ownship and ownshipGeometricAltitude 
	GDL-90 messages.
*/
func replaySituation(flight int64, db *sql.DB, timestamp int64) {
	
	var ts1, ts2 int64
	
	situationReplayComplete = false
	
	fields := "Lat, Lng, Pressure_alt, Alt, NACp, GroundSpeed, TrueCourse, timestamp_id"

	sql := fmt.Sprintf("SELECT %s FROM mySituation WHERE startup_id = %d AND timestamp_id > %d ORDER BY timestamp_id ASC;", fields, flight, timestamp)
	rows, err := db.Query(sql)
	if err != nil {
		fmt.Println("Error selecting data for replay of mySituation\n")
		return
	}
	
	defer rows.Close()
	
	for rows.Next() {
		
		if (ts1 == 0) {
			err = rows.Scan(&mySituation.Lat, &mySituation.Lng, &mySituation.Pressure_alt, &mySituation.Alt, &mySituation.NACp, &mySituation.GroundSpeed, &mySituation.TrueCourse, &ts1)
			if (err != nil) {
				return
			}
			continue
		}
		
		if (ts2 == 0) {
			err = rows.Scan(&mySituation.Lat, &mySituation.Lng, &mySituation.Pressure_alt, &mySituation.Alt, &mySituation.NACp, &mySituation.GroundSpeed, &mySituation.TrueCourse, &ts2)
			if (err != nil) {
				return
			}
		} 
		
		// wait for the appropriate number of ms
		var counter int64 = 0
		delta := (ts2 - ts1)
		wait := (delta / replaySpeed)
		
		// ignore dupes / noise
		if (wait) > 20 {
			
			for {
				time.Sleep(1 * time.Millisecond)
				counter++
				if abortReplay || (counter >= wait) {
					break;
				}
			}	
		}
		
		// update the replay status used by the websocket
		replayStatus.Timestamp = ts2
		
		// shuffle the timestamps
		ts1 = ts2
		ts2 = 0


		// don't do anything else - the ownship message should be sent out
		// by the heartBeatSender
		

		if pauseReplay {
			for {
				if (!pauseReplay) || (abortReplay) {
					break
				}
				time.Sleep(1 * time.Millisecond)
			}
		}

		if abortReplay {
			situationReplayComplete = true
			break
		}
	}

	situationReplayComplete = true
	if uatReplayComplete && esReplayComplete && situationReplayComplete {
		resetReplay()
	}
}


/*
listen for replay requests on the replay channel
	when a replay request arrives
	if we are already replaying,
		tell the play threads to stop
		wait for them to stop
	
	reset the paused flag
	create play threads passing flight, speed, offset
	
	wait for:
	new request
	terminate message
	
*/
func flightLogReplayThread() {

	var rr *ReplayData
	
	// open another connection to the database
	db, err := sql.Open("sqlite3", dataLogFilef)
	if err != nil {
		log.Printf("sql.Open(): %s\n", err.Error())
	}

	defer db.Close()
	
	for {
		
		if (rr != nil) {
			// if necessary, wait for an existing replay to stop
			if (globalStatus.ReplayMode) {
				abortReplay = true
				for {
					time.Sleep(10 * time.Millisecond)
					if (!globalStatus.ReplayMode) {
						break
					}
				}
			}
			
			// now start the next replay
			globalStatus.ReplayMode = true
			pauseReplay = false
			abortReplay = false
			replaySpeed = rr.Speed
			
			replayStatus.Speed = rr.Speed
			replayStatus.Flight = rr.Flight
			replayStatus.Timestamp = rr.Timestamp
			
			go replayUAT(rr.Flight, db, rr.Timestamp)
			go replay1090(rr.Flight, db, rr.Timestamp)
			go replaySituation(rr.Flight, db, rr.Timestamp)
		}
		
		// wait for another request
		select {
		case rp, ok := <-replayChan:
			if (ok) {
				rr = &rp
			} else {
				return
			}
		}
	}
}

func replayFlightLog(flight int64, speed int64, timestamp int64) {
	
	var replay ReplayData
	replay.Flight = flight
	replay.Timestamp = timestamp
	replay.Speed = speed
	
	replayStatus.Flight = flight
	replayStatus.Speed = speed
	replayStatus.Timestamp = timestamp
	
	replayChan <- replay
}


/*
	updateFlightLog(): updates the SQLite record for the current startup to indicate
	the appropriate starting and ending values. This is called by dataLogWriter() on
	its thread (it is a go routine) when the dataUpdateChan is flagged.
	
	TODO: replace this with a reflective / introspective automatic update routine ala
	the insert routine used for bulk updates.
*/
func updateFlightLog(db *sql.DB) {
	
	var sql string
	sql = "UPDATE `startup` SET\n"
	sql = sql + "start_airport_id = ?,\n"
	sql = sql + "start_airport_name = ?,\n"
	sql = sql + "start_timestamp = ?,\n"
	sql = sql + "start_localtime = ?,\n"
	sql = sql + "start_tz = ?,\n"
	sql = sql + "start_lat = ?,\n"
	sql = sql + "start_lng = ?,\n"
	sql = sql + "start_alt = ?,\n"
	sql = sql + "end_airport_id = ?,\n"
	sql = sql + "end_airport_name = ?,\n"
	sql = sql + "end_timestamp = ?,\n"
	sql = sql + "end_localtime = ?,\n"
	sql = sql + "end_tz = ?,\n"
	sql = sql + "end_lat = ?,\n"
	sql = sql + "end_lng = ?,\n"
	sql = sql + "max_alt = ?,\n"
	sql = sql + "duration = ?,\n"
	sql = sql + "distance = ?,\n"
	sql = sql + "groundspeed = ?,\n"
	sql = sql + "route = ?\n"
	sql = sql + "WHERE id = ?;"
	
	stmt, err := db.Prepare(sql)
	if err != nil {
		fmt.Printf("Error creating statement: %v", err)
		return
	}
	
	f := flightlog
	ret, err := stmt.Exec(f.start_airport_id, f.start_airport_name, f.start_timestamp, f.start_localtime, f.start_tz, f.start_lat, f.start_lng, f.start_alt, f.end_airport_id, f.end_airport_name, f.end_timestamp, f.end_localtime, f.end_tz, f.end_lat, f.end_lng, f.max_alt, f.duration, f.distance, f.groundspeed, f.route, stratuxStartupID)
	if err != nil {
		fmt.Printf("Error executing statement: %v\n", err)
		return
	}
	raf, err := ret.RowsAffected()
	if raf < 1 {
		fmt.Println("Error - no rows affected in update")
	}
}

/*
	startFlightLog() - called once per startup when the GPS has a valid timestamp and
	position to tag the beginning of the 'session'. Updates the startup record with
	the initial place / time values.
*/
func startFlightLog() {

	// gps coordinates at startup
	flightlog.start_lat = float64(mySituation.Lat)
	flightlog.start_lng = float64(mySituation.Lng)
	flightlog.start_alt = mySituation.Alt
	flightlog.max_alt = mySituation.Alt
	
	// time, timezone, localtime
	flightlog.start_timestamp = (stratuxClock.RealTime.UnixNano() / 1000000)
	flightlog.start_tz = latlong.LookupZoneName(float64(mySituation.Lat), float64(mySituation.Lng))
	loc, err := time.LoadLocation(flightlog.start_tz)
	if (err == nil) {
		flightlog.start_localtime = stratuxClock.RealTime.In(loc).String()
	}
	
	// airport code and name
	apt, err := findAirport(float64(mySituation.Lat), float64(mySituation.Lng))
	if (err == nil) {
		flightlog.start_airport_id = apt.faaId
		flightlog.start_airport_name = apt.name
		flightlog.route = apt.faaId
	}
	
	// update the database entry
	dataUpdateChan <- true
}

/*
	stopFlightLog() - called every time the system shifts from "flying" state to "taxiing"
	state (or directly to stopped, though that should not happen). Updates the end values
	for the startup record. Appends the stop point airport to the 'route' list, so if
	the aircraft makes multiple stops without powering off the system this will indicate
	all of them.
*/
func stopFlightLog(fullstop bool) {

	// gps coordinates at startup
	flightlog.end_lat = float64(mySituation.Lat)
	flightlog.end_lng = float64(mySituation.Lng)
	
	// time, timezone, localtime
	flightlog.end_timestamp = stratuxClock.RealTime.Unix()
	flightlog.end_tz = latlong.LookupZoneName(float64(mySituation.Lat), float64(mySituation.Lng))
	loc, err := time.LoadLocation(flightlog.end_tz)
	if (err == nil) {
		flightlog.end_localtime = mySituation.GPSTime.In(loc).String()
	}
	
	// airport code and name
	apt, err := findAirport(float64(mySituation.Lat), float64(mySituation.Lng))
	if (err == nil) {
		flightlog.end_airport_id = apt.faaId
		flightlog.end_airport_name = apt.name
		flightlog.route = flightlog.route + " => " + apt.faaId
		if (fullstop == false) {
			flightlog.route = flightlog.route + " (t/g)"
		}
	}
	
	//create a landing record in the event log table
	if (fullstop == false) {
		addFlightEvent("Landing (T/G)")
	} else {
		addFlightEvent("Landing")
	}
	
	// update the database entry
	dataUpdateChan <- true
}

/*
	append a flight event record to the 'events' table in the database
*/
func addFlightEvent(event string) {
	
	var myEvent FlightEvent
	myEvent.event = event
	myEvent.lat = float64(mySituation.Lat)
	myEvent.lng = float64(mySituation.Lng)
	
	
	timezone := latlong.LookupZoneName(float64(mySituation.Lat), float64(mySituation.Lng))
	loc, err := time.LoadLocation(timezone)
	if (err == nil) {
		lt := stratuxClock.RealTime.In(loc)
		myEvent.localtime = lt.Format("15:04:05 MST") 
	}
	
	apt, err := findAirport(float64(mySituation.Lat), float64(mySituation.Lng))
	if (err == nil) {
		myEvent.airport_id = apt.faaId
		myEvent.airport_name = apt.name
	}	
	
	myEvent.timestamp = stratuxClock.RealTime.Unix()
	
	dataLogChan <- DataLogRow{tbl: "events", data: myEvent}
}

/*
	logSituation() - pushes the current 'mySituation' record into the logging channel
	for writing to the SQLite database. Also provides triggers for startFlightLog(),
	stopFlightLog() and updates the running distance and time tallies for the flight.
	
	FWIW - this requires a valid GPS value and a valid, real time value. Without those,
	situation records are pretty well worthless anyway.
*/

func logSituation() {
	if globalSettings.ReplayLog && isDataLogReady() && (globalStatus.ReplayMode == false) {
		
		// make sure we have valid GPS Clock time
		if (flightlog.start_timestamp == 0) {
			if (isGPSValid() && stratuxClock.HasRealTimeReference()) {
				startFlightLog()
			} else {
				// not initialized / can't initialize yet - no clock
				return
			}
		}
		
		// update the amount of time since startup in seconds
		flightlog.duration = int64(stratuxClock.Milliseconds / 1000)
		

		// get the current flight state
		var flightState int = FLIGHT_STATE_UNKNOWN

		// if we are stopped and the gps detects that we are moving faster than 5 mph, then we are taxiing
		if ((flightState0 == FLIGHT_STATE_STOPPED) || (flightState0 == FLIGHT_STATE_UNKNOWN)) && ((mySituation.GroundSpeed > startTaxiingSpeed) && (mySituation.GroundSpeed <= startFlyingSpeed)) {
			flightState = FLIGHT_STATE_TAXIING
		} else

		// if we are taxiing and the gps detects that we are moving faster than 60 mph, then we are flying
		if ((flightState0 == FLIGHT_STATE_TAXIING) || (flightState0 == FLIGHT_STATE_UNKNOWN)) && (mySituation.GroundSpeed > startFlyingSpeed) {
			flightState = FLIGHT_STATE_FLYING
		} else
		
		// if we are taxiing and the gps detects that we are moving 0 mph, then we are stopped
		if (flightState0 == FLIGHT_STATE_TAXIING) && (mySituation.GroundSpeed <= stopTaxiingSpeed) {
			flightState = FLIGHT_STATE_STOPPED
		} else

		// if we are flying and the gps detects that we are moving less than 50 mph, then we are taxiing
		if (flightState0 == FLIGHT_STATE_FLYING) && (mySituation.GroundSpeed <= stopFlyingSpeed) {
			flightState = FLIGHT_STATE_TAXIING
		} else

		// non-transitional states
		if (mySituation.GroundSpeed > startFlyingSpeed) {
			flightState = FLIGHT_STATE_FLYING
		} else
		if (mySituation.GroundSpeed > startTaxiingSpeed) {
			flightState = FLIGHT_STATE_TAXIING
		} else {
			flightState = FLIGHT_STATE_STOPPED
		}
		
		
		// look for a transition
		if (flightState != flightState0) {
		
			// shuffle the flight state values
			flightState2 = flightState1
			flightState1 = flightState0
			flightState0 = flightState
			
			// look for event patterns in the past three states
			switch true {
			case (flightState2 == FLIGHT_STATE_UNKNOWN) && (flightState1 == FLIGHT_STATE_UNKNOWN) && (flightState0 == FLIGHT_STATE_STOPPED):
				// normal startup - do nothing
				addFlightEvent("Startup")
				
			case (flightState2 == FLIGHT_STATE_UNKNOWN) && (flightState1 == FLIGHT_STATE_UNKNOWN) && (flightState0 == FLIGHT_STATE_TAXIING):
				// rolling startup or restart - ??
				fmt.Printf("Detected restart or delayed start while taxiing: %s\n", stratuxClock.RealTime.String())
				addFlightEvent("Restart")
				
			case (flightState2 == FLIGHT_STATE_UNKNOWN) && (flightState1 == FLIGHT_STATE_UNKNOWN) && (flightState0 == FLIGHT_STATE_FLYING):
				// flying startup or restart - ??
				fmt.Printf("Detected restart or delayed start while flying: %s\n", stratuxClock.RealTime.String())
				addFlightEvent("Restart")
				
			case (flightState2 == FLIGHT_STATE_UNKNOWN) && (flightState1 == FLIGHT_STATE_STOPPED) && (flightState0 == FLIGHT_STATE_TAXIING):
				// normal taxi-out
				addFlightEvent("Taxiing")
				
			case (flightState2 == FLIGHT_STATE_STOPPED) && (flightState1 == FLIGHT_STATE_TAXIING) && (flightState0 == FLIGHT_STATE_STOPPED):
				// local reposition - do nothing
				addFlightEvent("Stopped")
				
			case (flightState2 == FLIGHT_STATE_TAXIING) && (flightState1 == FLIGHT_STATE_STOPPED) && (flightState0 == FLIGHT_STATE_TAXIING):
				// just more taxiing - ignore
				addFlightEvent("Taxiing")
				
			case (flightState2 == FLIGHT_STATE_STOPPED) && (flightState1 == FLIGHT_STATE_TAXIING) && (flightState0 == FLIGHT_STATE_FLYING):
				// normal takeoff
				addFlightEvent("Takeoff")
				
			case (flightState2 == FLIGHT_STATE_TAXIING) && (flightState1 == FLIGHT_STATE_FLYING) && (flightState0 == FLIGHT_STATE_TAXIING):
				// touchdown
				addFlightEvent("Touchdown")
				
			case (flightState2 == FLIGHT_STATE_FLYING) && (flightState1 == FLIGHT_STATE_TAXIING) && (flightState0 == FLIGHT_STATE_FLYING):
				// touch and go - landing + takeoff
				stopFlightLog(false)
				addFlightEvent("Takeoff")
				
			case (flightState2 == FLIGHT_STATE_FLYING) && (flightState1 == FLIGHT_STATE_TAXIING) && (flightState0 == FLIGHT_STATE_STOPPED):
				// full-stop landing
				stopFlightLog(true)
			}
		}
		
		// update altitude value - used for determining "real" flights vs non-flight startups
		if (mySituation.Alt > flightlog.max_alt) {
			flightlog.max_alt = mySituation.Alt
		}
		
		// if log level is anything less than DEMO (3), we want to limit the update frequency
		if globalSettings.FlightLogLevel < FLIGHT_LOG_LEVEL_DEMO {
			now := stratuxClock.Milliseconds
			msd := (now - lastSituationLogMs)
		
			// logbook is 30 seconds (30,000 ms)
			if (globalSettings.FlightLogLevel == FLIGHT_LOG_LEVEL_LOGBOOK) && (msd < 30000) {
				return;
			}
			
			// debrief is 2 Hz (500 ms)
			if (globalSettings.FlightLogLevel == FLIGHT_LOG_LEVEL_DEBRIEF) && (msd < 500) {
				return;
			}
			
		}
		
		// only bother to write records if we are moving somehow
		if (flightState0 == FLIGHT_STATE_FLYING) || (flightState0 == FLIGHT_STATE_TAXIING) {
		
			dataLogChan <- DataLogRow{tbl: "mySituation", data: mySituation}
			
			// update the distance traveled in nautical miles
			p := geo.NewPoint(float64(mySituation.Lat), float64(mySituation.Lng))
			if (lastPoint != nil) {
				segment := p.GreatCircleDistance(lastPoint);
				flightlog.distance = flightlog.distance + (segment * NM_PER_KM)
			}
			lastPoint = p;
		}
		
		// update the current startup record in the database every 60 seconds
		if (flightlog.duration % 60) == 0 {
			dataUpdateChan <- true
		}
		
		lastSituationLogMs = stratuxClock.Milliseconds
	}
}

func logStatus() {
	if globalSettings.ReplayLog && isDataLogReady() && (globalStatus.ReplayMode == false) {
		dataLogChan <- DataLogRow{tbl: "status", data: globalStatus}
	}
}

func logSettings() {
	if globalSettings.ReplayLog && isDataLogReady() && (globalStatus.ReplayMode == false) {
		dataLogChan <- DataLogRow{tbl: "settings", data: globalSettings}
	}
}

func logTraffic(ti TrafficInfo) {
	if globalSettings.ReplayLog && isDataLogReady() && (globalSettings.FlightLogLevel == FLIGHT_LOG_LEVEL_DEBUG) && (globalStatus.ReplayMode == false) {
		dataLogChan <- DataLogRow{tbl: "traffic", data: ti}
	}
}

func logMsg(m msg) {
	if globalSettings.ReplayLog && isDataLogReady() && (globalSettings.FlightLogLevel > FLIGHT_LOG_LEVEL_DEBRIEF) && (globalStatus.ReplayMode == false) && (flightState0 == FLIGHT_STATE_FLYING) {
		dataLogChan <- DataLogRow{tbl: "messages", data: m}
	}
}

func logESMsg(m esmsg) {
	if globalSettings.ReplayLog && isDataLogReady() && (globalSettings.FlightLogLevel > FLIGHT_LOG_LEVEL_DEBRIEF) && (globalStatus.ReplayMode == false) && (flightState0 == FLIGHT_STATE_FLYING) {
		dataLogChan <- DataLogRow{tbl: "es_messages", data: m}
	}
}

func logDump1090TermMessage(m Dump1090TermMessage) {
	if globalSettings.DEBUG && globalSettings.ReplayLog && isDataLogReady() && (globalSettings.FlightLogLevel == FLIGHT_LOG_LEVEL_DEBUG) && (globalStatus.ReplayMode == false) {
		dataLogChan <- DataLogRow{tbl: "dump1090_terminal", data: m}
	}
}

func initDataLog() {
	//log.Printf("dataLogStarted = %t. dataLogReadyToWrite = %t\n", dataLogStarted, dataLogReadyToWrite) //REMOVE -- DEBUG
	insertString = make(map[string]string)
	insertBatchIfs = make(map[string][][]interface{})
	go dataLogWatchdog()
	//log.Printf("datalog.go: initDataLog() complete.\n") //REMOVE -- DEBUG
	
	replayChan = make(chan ReplayData)
	go flightLogReplayThread()
}

/*
	dataLogWatchdog(): Watchdog function to control startup / shutdown of data logging subsystem.
		Called by initDataLog as a goroutine. It iterates once per second to determine if
		globalSettings.ReplayLog has toggled. If logging was switched from off to on, it starts
		datalog() as a goroutine. If the log is running and we want it to stop, it calls
		closeDataLog() to turn off the input channels, close the log, and tear down the dataLog
		and dataLogWriter goroutines.
*/

func dataLogWatchdog() {
	for {
		if !dataLogStarted && globalSettings.ReplayLog { // case 1: sqlite logging isn't running, and we want to start it
			log.Printf("datalog.go: Watchdog wants to START logging.\n")
			go dataLog()
		} else if dataLogStarted && !globalSettings.ReplayLog { // case 2:  sqlite logging is running, and we want to shut it down
			log.Printf("datalog.go: Watchdog wants to STOP logging.\n")
			closeDataLog()
		}
		//log.Printf("Watchdog iterated.\n") //REMOVE -- DEBUG
		time.Sleep(1 * time.Second)
		//log.Printf("Watchdog sleep over.\n") //REMOVE -- DEBUG
	}
}

/*
	closeDataLog(): Handler for graceful shutdown of data logging goroutines. It is called by
		by dataLogWatchdog(), gracefulShutdown(), and by any other function (disk space monitor?)
		that needs to be able to shut down sqlite logging without corrupting data or blocking
		execution.

		This function turns off log message reads into the dataLogChan receiver, and sends a
		message to a quit channel ('shutdownDataLogWriter`) in dataLogWriter(). dataLogWriter()
		then sends a message to a quit channel to 'shutdownDataLog` in dataLog() to close *that*
		goroutine. That function sets dataLogStarted=false once the logfile is closed. By waiting
		for that signal, closeDataLog() won't exit until the log is safely written. This prevents
		data loss on shutdown.
*/

func closeDataLog() {
	//log.Printf("closeDataLog(): dataLogStarted = %t\n", dataLogStarted) //REMOVE -- DEBUG
	dataLogReadyToWrite = false // prevent any new messages from being sent down the channels
	log.Printf("datalog.go: Starting data log shutdown\n")
	shutdownDataLogWriter <- true      //
	defer close(shutdownDataLogWriter) // ... and close the channel so subsequent accidental writes don't stall execution
	log.Printf("datalog.go: Waiting for shutdown signal from dataLog()")
	for dataLogStarted {
		//log.Printf("closeDataLog(): dataLogStarted = %t\n", dataLogStarted) //REMOVE -- DEBUG
		time.Sleep(50 * time.Millisecond)
	}
	log.Printf("datalog.go: Data log shutdown successful.\n")
}
