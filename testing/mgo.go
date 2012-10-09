package testing

import (
	"fmt"
	"io/ioutil"
	"labix.org/v2/mgo"
	. "launchpad.net/gocheck"
	"net"
	"os"
	"os/exec"
	"strconv"
	stdtesting "testing"
	"time"
)

// MgoAddr holds the address of the shared MongoDB server set up by
// StartMgoServer.
var MgoAddr string

// mgoSession holds an admin-authenticated connection to the above mongo
// address so that we can manipulate the database even when
// authentication has been set up during a test.
var mgoSession *mgo.Session

// MgoSuite is a suite that deletes all content from the shared MongoDB
// server at the end of every test and supplies a connection to the shared
// MongoDB server.
type MgoSuite struct {
	Session *mgo.Session
}

// StartMgoServer starts a MongoDB server in a temporary directory.
// It panics if it encounters an error.
func StartMgoServer() (server *exec.Cmd, dbdir string, err error) {
	dbdir, err = ioutil.TempDir("", "test-mgo")
	if err != nil {
		return
	}
	mgoport := strconv.Itoa(FindTCPPort())
	mgoargs := []string{
		"--auth",
		"--dbpath", dbdir,
		"--bind_ip", "localhost",
		"--port", mgoport,
		"--nssize", "1",
		"--noprealloc",
		"--smallfiles",
		"--nojournal",
	}
	server = exec.Command("mongod", mgoargs...)
	err = server.Start()
	if err != nil {
		os.RemoveAll(dbdir)
		return
	}
	MgoAddr = "localhost:" + mgoport
	// Give ourselves a logged in session so that we can manipulate
	// the db even when an admin password has been set.
	session := MgoDial()
	admin := session.DB("admin")
	if err := admin.AddUser("admin", "foo", false); err != nil && err.Error() != "need to login" {
		panic(fmt.Errorf("cannot add admin user: %v", err))
	}
	if err := admin.Login("admin", "foo"); err != nil {
		panic(fmt.Errorf("cannot login after setting password: %v", err))
	}
	if err := admin.RemoveUser("admin"); err != nil {
		panic(fmt.Errorf("cannot remove admin user: %v", err))
	}
	mgoSession = session
	return
}

func MgoDestroy(server *exec.Cmd, dbdir string) {
	mgoSession.Close()
	server.Process.Kill()
	server.Process.Wait()
	os.RemoveAll(dbdir)
}

// MgoTestPackage should be called to register the tests for any package that
// requires a MongoDB server.
func MgoTestPackage(t *stdtesting.T) {
	server, dbdir, err := StartMgoServer()
	if err != nil {
		t.Fatal(err)
	}
	defer MgoDestroy(server, dbdir)
	TestingT(t)
}

func (s *MgoSuite) SetUpSuite(c *C) {
	if MgoAddr == "" {
		panic("MgoSuite tests must be run with MgoTestPackage")
	}
	mgo.SetStats(true)
}

func (s *MgoSuite) TearDownSuite(c *C) {}

// MgoDial returns a new connection to the shared MongoDB server.
func MgoDial() *mgo.Session {
	session, err := mgo.Dial(MgoAddr)
	if err != nil {
		panic(err)
	}
	return session
}

func (s *MgoSuite) SetUpTest(c *C) {
	mgo.ResetStats()
	s.Session = MgoDial()
}

// MgoReset deletes all content from the shared MongoDB server.
func MgoReset() {
	dbnames, err := mgoSession.DatabaseNames()
	if err != nil {
		panic(err)
	}
	for _, name := range dbnames {
		switch name {
		case "admin", "local", "config":
		default:
			err = mgoSession.DB(name).DropDatabase()
			if err != nil {
				panic(fmt.Errorf("Cannot drop MongoDB database %v: %v", name, err))
			}
		}
	}
	// TODO(rog) remove all admin users when mgo provides a facility to list them,
	err = mgoSession.DB("admin").RemoveUser("admin")
	if err != nil && err != mgo.ErrNotFound {
		panic(err)
	}
}

func (s *MgoSuite) TearDownTest(c *C) {
	MgoReset()
	s.Session.Close()
	for i := 0; ; i++ {
		stats := mgo.GetStats()
		if stats.SocketsInUse <= 1 && stats.SocketsAlive <= 1 {
			break
		}
		if i == 20 {
			c.Fatal("Test left sockets in a dirty state")
		}
		c.Logf("Waiting for sockets to die: %d in use, %d alive", stats.SocketsInUse, stats.SocketsAlive)
		time.Sleep(500 * time.Millisecond)
	}
}

// FindTCPPort finds an unused TCP port and returns it.
// Use of this function has an inherent race condition - another
// process may claim the port before we try to use it.
// We hope that the probability is small enough during
// testing to be negligible.
func FindTCPPort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
