// monitor.go
package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/mariadb-corporation/replication-manager/alert"
	"github.com/mariadb-corporation/replication-manager/gtid"
	"github.com/tanji/mariadb-tools/dbhelper"
)

// ServerMonitor defines a server to monitor.
type ServerMonitor struct {
	Conn                 *sqlx.DB
	URL                  string
	Host                 string
	Port                 string
	IP                   string
	BinlogPos            *gtid.List
	Strict               string
	ServerID             uint
	MasterServerID       uint
	MasterHost           string
	LogBin               string
	UsingGtid            string
	CurrentGtid          *gtid.List
	SlaveGtid            *gtid.List
	IOThread             string
	SQLThread            string
	ReadOnly             string
	Delay                sql.NullInt64
	State                string
	PrevState            string
	IOErrno              uint
	IOError              string
	SQLErrno             uint
	SQLError             string
	FailCount            int
	SemiSyncMasterStatus bool
	RplMasterStatus      bool
}

type serverList []*ServerMonitor

var maxConn string

const (
	stateFailed  string = "Failed"
	stateMaster  string = "Master"
	stateSlave   string = "Slave"
	stateUnconn  string = "Unconnected"
	stateSuspect string = "Suspect"
)

/* Initializes a server object */
func newServerMonitor(url string) (*ServerMonitor, error) {
	server := new(ServerMonitor)
	server.URL = url
	server.Host, server.Port = splitHostPort(url)
	var err error
	server.IP, err = dbhelper.CheckHostAddr(server.Host)
	if err != nil {
		errmsg := fmt.Errorf("ERROR: DNS resolution error for host %s", server.Host)
		return server, errmsg
	}
	params := fmt.Sprintf("?timeout=%ds", timeout)
	mydsn := func() string {
		dsn := dbUser + ":" + dbPass + "@"
		if server.Host != "" {
			dsn += "tcp(" + server.Host + ":" + server.Port + ")/" + params
		} else {
			dsn += "unix(" + socket + ")/" + params
		}
		return dsn
	}
	server.Conn, err = sqlx.Open("mysql", mydsn())
	return server, err
}

func (server *ServerMonitor) check(wg *sync.WaitGroup) {

	defer wg.Done()

	if server.PrevState != server.State {
		server.PrevState = server.State
	}

	var err error
	switch checktype {
	case "tcp":
		err = server.Conn.Ping()
	case "agent":
		var resp *http.Response
		resp, err = http.Get("http://" + server.Host + ":10001/check/")
		if resp.StatusCode != 200 {
			// if 404, consider server down or agent killed. Don't initiate anything
			err = fmt.Errorf("HTTP Response Code Error: %d", resp.StatusCode)
		}
	}

	// Handle failure cases here
	if err != nil {
		if err != sql.ErrNoRows && (server.State == stateMaster || server.State == stateSuspect) {
			server.FailCount++
			if server.URL == master.URL {
				if master.FailCount <= maxfail {
					logprintf("WARN : Master Failure detected! Retry %d/%d", master.FailCount, maxfail)
				}
				if server.FailCount >= maxfail {
					if server.FailCount == maxfail {
						logprint("WARN : Declaring master as failed")
					}
					master.State = stateFailed
				} else {
					master.State = stateSuspect
				}
			}
		} else {
			if server.State != stateMaster && server.State != stateFailed {
				server.FailCount++
				if server.FailCount >= maxfail {
					if server.FailCount == maxfail {
						logprintf("WARN : Declaring server %s as failed", server.URL)
						server.State = stateFailed
					} else {
						server.State = stateSuspect
					}
					// remove from slave list
					server.delete(&slaves)
				}
			}
		}
		// Send alert if state has changed
		if server.PrevState != server.State && mailTo != "" {
			if verbose {
				logprintf("INFO : Server %s state changed from %s to %s", server.URL, server.PrevState, server.State)
			}
			a := alert.Alert{
				From:        mailFrom,
				To:          mailTo,
				Type:        server.State,
				Origin:      server.URL,
				Destination: mailSMTPAddr,
			}
			err = a.Email()
			if err != nil {
				logprint("ERROR: Could not send email alert: ", err)
			}
		}
		return
	}

	// uptime monitoring
	if server.State == stateMaster {
		sme.SetMasterUpAndSync(server.SemiSyncMasterStatus, server.RplMasterStatus)
	}

	_, err = dbhelper.GetSlaveStatus(server.Conn)
	if err == sql.ErrNoRows {
		// If we reached this stage with a previously failed server, reintroduce
		// it as unconnected server.
		if server.PrevState == stateFailed {
			if loglevel > 1 {
				logprintf("DEBUG: State comparison reinitialized failed server %s as unconnected", server.URL)
			}
			server.State = stateUnconn
			server.FailCount = 0
			if autorejoin {
				// Check if master exists in topology before rejoining.
				if server.URL != master.URL {
					logprintf("INFO : Rejoining previously failed server %s", server.URL)
					err = server.rejoin()
					if err != nil {
						logprintf("ERROR: Failed to autojoin previously failed server %s", server.URL)
					}
				}
			}
		} else if server.State != stateMaster {
			if loglevel > 1 {
				logprintf("DEBUG: State unconnected set by non-master rule on server %s", server.URL)
			}
			server.State = stateUnconn
		}
		return
	}

	// In case of state change, reintroduce the server in the slave list
	if server.PrevState == stateFailed || server.PrevState == stateUnconn {
		server.State = stateSlave
		server.FailCount = 0
		slaves = append(slaves, server)
		if readonly {
			err = dbhelper.SetReadOnly(server.Conn, true)
			if err != nil {
				logprintf("ERROR: Could not set rejoining slave %s as read-only, %s", server.URL, err)
			}
		}
	}
}

/* Refresh a server object */
func (server *ServerMonitor) refresh() error {
	err := server.Conn.Ping()
	if err != nil {
		return err
	}
	sv, err := dbhelper.GetVariables(server.Conn)
	if err != nil {
		return err
	}
	server.BinlogPos = gtid.NewList(sv["GTID_BINLOG_POS"])
	server.Strict = sv["GTID_STRICT_MODE"]
	server.LogBin = sv["LOG_BIN"]
	server.ReadOnly = sv["READ_ONLY"]
	server.CurrentGtid = gtid.NewList(sv["GTID_CURRENT_POS"])
	server.SlaveGtid = gtid.NewList(sv["GTID_SLAVE_POS"])
	sid, _ := strconv.ParseUint(sv["SERVER_ID"], 10, 0)
	server.ServerID = uint(sid)
	err = dbhelper.SetDefaultMasterConn(server.Conn, masterConn)
	if err != nil {
		return err
	}

	su := dbhelper.GetStatus(server.Conn)
	if su["RPL_SEMI_SYNC_MASTER_STATUS"] == "ON" {
		server.SemiSyncMasterStatus = true
	} else {
		server.SemiSyncMasterStatus = false
	}

	slaveStatus, err := dbhelper.GetSlaveStatus(server.Conn)
	if err != nil {
		server.UsingGtid = ""
		server.IOThread = ""
		server.SQLThread = ""
		server.Delay = sql.NullInt64{Int64: 0, Valid: false}
		server.MasterServerID = 0
		server.MasterHost = ""
		server.IOErrno = 0
		server.IOError = ""
		server.SQLError = ""
		server.SQLErrno = 0
		return nil
	}
	server.UsingGtid = slaveStatus.Using_Gtid
	server.IOThread = slaveStatus.Slave_IO_Running
	server.SQLThread = slaveStatus.Slave_SQL_Running
	server.Delay = slaveStatus.Seconds_Behind_Master
	server.MasterServerID = slaveStatus.Master_Server_Id
	server.MasterHost = slaveStatus.Master_Host
	server.IOErrno = slaveStatus.Last_IO_Errno
	server.IOError = slaveStatus.Last_IO_Error
	server.SQLError = slaveStatus.Last_SQL_Error
	server.SQLErrno = slaveStatus.Last_SQL_Errno

	return nil
}

/* Check replication health and return status string */
func (server *ServerMonitor) healthCheck() string {
	if server.State == stateMaster {
		return "Master OK"
	}
	if server.Delay.Valid == false {
		if server.SQLThread == "Yes" && server.IOThread == "No" {
			return fmt.Sprintf("NOT OK, IO Stopped (%d)", server.IOErrno)
		} else if server.SQLThread == "No" && server.IOThread == "Yes" {
			return fmt.Sprintf("NOT OK, SQL Stopped (%d)", server.SQLErrno)
		} else {
			return "NOT OK, ALL Stopped"
		}
	} else {
		if server.Delay.Int64 > 0 {
			return "Behind master"
		}
		return "Running OK"
	}
}

/* Handles write freeze and existing transactions on a server */
func (server *ServerMonitor) freeze() bool {
	err := dbhelper.SetReadOnly(server.Conn, true)
	if err != nil {
		logprintf("WARN : Could not set %s as read-only: %s", server.URL, err)
		return false
	}
	for i := waitKill; i > 0; i -= 500 {
		threads := dbhelper.CheckLongRunningWrites(server.Conn, 0)
		if threads == 0 {
			break
		}
		logprintf("INFO : Waiting for %d write threads to complete on %s", threads, server.URL)
		time.Sleep(500 * time.Millisecond)
	}
	maxConn = dbhelper.GetVariableByName(server.Conn, "MAX_CONNECTIONS")
	_, err = server.Conn.Exec("SET GLOBAL max_connections=0")
	logprintf("INFO : Terminating all threads on %s", server.URL)
	dbhelper.KillThreads(server.Conn)
	return true
}

/* Returns a candidate from a list of slaves. If there's only one slave it will be the de facto candidate. */
func (server *ServerMonitor) electCandidate(l []*ServerMonitor) int {
	ll := len(l)
	if verbose {
		logprintf("DEBUG: Processing %d candidates", ll)
	}
	seqList := make([]uint64, ll)
	hiseq := 0
	var max uint64
	for i, sl := range l {
		/* If server is in the ignore list, do not elect it */
		if contains(ignoreList, sl.URL) {
			if verbose {
				logprintf("DEBUG: %s is in the ignore list. Skipping", sl.URL)
			}
			continue
		}
		// Refresh state before evaluating
		sl.refresh()
		if server.State != stateFailed || server.State == stateMaster {
			if verbose {
				logprintf("DEBUG: Checking eligibility of slave server %s [%d]", sl.URL, i)
			}
			if multiMaster == true && sl.State == stateMaster {
				logprintf("WARN : Slave %s has state Master. Skipping", sl.URL)
				continue
			}
			if dbhelper.CheckSlavePrerequisites(sl.Conn, sl.Host) == false {
				continue
			}
			if dbhelper.CheckBinlogFilters(server.Conn, sl.Conn) == false {
				logprintf("WARN : Binlog filters differ on master and slave %s. Skipping", sl.URL)
				continue
			}
			if dbhelper.CheckReplicationFilters(server.Conn, sl.Conn) == false {
				logprintf("WARN : Replication filters differ on master and slave %s. Skipping", sl.URL)
				continue
			}
			ss, _ := dbhelper.GetSlaveStatus(sl.Conn)
			if ss.Seconds_Behind_Master.Valid == false {
				logprintf("WARN : Slave %s is stopped. Skipping", sl.URL)
				continue
			}
			if ss.Seconds_Behind_Master.Int64 > maxDelay {
				logprintf("WARN : Slave %s has more than %d seconds of replication delay (%d). Skipping", sl.URL, maxDelay, ss.Seconds_Behind_Master.Int64)
				continue
			}
			if gtidCheck && dbhelper.CheckSlaveSync(sl.Conn, server.Conn) == false {
				logprintf("WARN : Slave %s not in sync. Skipping", sl.URL)
				continue
			}
		}
		/* Rig the election if the examined slave is preferred candidate master */
		if sl.URL == prefMaster {
			if verbose {
				logprintf("DEBUG: Election rig: %s elected as preferred master", sl.URL)
			}
			return i
		}
		seqnos := sl.SlaveGtid.GetSeqNos()
		if verbose {
			logprintf("DEBUG: Got sequence(s) %v for server [%d]", seqnos, i)
		}
		for _, v := range seqnos {
			seqList[i] += v
		}
		if seqList[i] > max {
			max = seqList[i]
			hiseq = i
		}
	}
	if max > 0 {
		/* Return key of slave with the highest seqno. */
		return hiseq
	}
	logprint("ERROR: No suitable candidates found.")
	return -1
}

func (server *ServerMonitor) log() {
	server.refresh()
	logprintf("DEBUG: Server:%s Current GTID:%s Slave GTID:%s Binlog Pos:%s", server.URL, server.CurrentGtid.Sprint(), server.SlaveGtid.Sprint(), server.BinlogPos.Sprint())
	return
}

func (server *ServerMonitor) close() {
	server.Conn.Close()
	return
}

func (server *ServerMonitor) writeState() error {
	server.log()
	f, err := os.Create("/tmp/repmgr.state")
	if err != nil {
		return err
	}
	_, err = f.WriteString(server.BinlogPos.Sprint())
	if err != nil {
		return err
	}
	return nil
}

func (server *ServerMonitor) hasSiblings(sib []*ServerMonitor) bool {
	for _, sl := range sib {
		if server.MasterServerID != sl.MasterServerID {
			return false
		}
	}
	return true
}

func (server *ServerMonitor) delete(sl *serverList) {
	lsm := *sl
	for k, s := range lsm {
		if server.URL == s.URL {
			lsm[k] = lsm[len(lsm)-1]
			lsm[len(lsm)-1] = nil
			lsm = lsm[:len(lsm)-1]
			break
		}
	}
	*sl = lsm
}

func (server *ServerMonitor) rejoin() error {
	if readonly {
		dbhelper.SetReadOnly(server.Conn, true)
	}
	cm := "CHANGE MASTER TO master_host='" + master.IP + "', master_port=" + master.Port + ", master_user='" + rplUser + "', master_password='" + rplPass + "', MASTER_USE_GTID=CURRENT_POS"
	_, err := server.Conn.Exec(cm)
	dbhelper.StartSlave(server.Conn)
	return err
}
