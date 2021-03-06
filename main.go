package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
    "regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulstuart/secrets"
	ssh "github.com/paulstuart/sshclient"
	"github.com/spf13/viper"
)

var (
	version        = "0.0.1"
	sessionMinutes = time.Duration(time.Minute * 240)
	masterMode     = true
	hostname, _    = os.Hostname()
	basedir, _     = os.Getwd() // get abs path now, as we will be changing dirs
	execDir, _     = absExecPath()
	startTime      = time.Now()
	sqlDir         = "sql" // dir containing sql schemas, etc
	sqlSchema      = sqlDir + "/schema.sql"
	dbFile     = execDir + "/data.db"
	udpChan    = make(chan []byte, 1024)
	closer     = make(chan struct{})
	macHosts   = make(map[string]string)
	ipmiHosts   = make(map[string]string) // ipmi ip -> hostname
	sshTimeout = 20
	sshUser    string
	sshKeyFile string
	sshKey     string
	dcmanURL   string
	dcmanKey   string
	httpPort   int
	udpPort    int
	logDir     string
	samlURL    string
	loginURL   string
	oktaToken  string
	oktaCookie string
	oktaHash   string
	pLock      sync.Mutex
	macLock    sync.Mutex
	aLock      sync.Mutex                  // active lock
	iLock      sync.Mutex                  // active lock
	activeMACs = make(map[string]struct{}) // db of active mac addresses (between boot request and reboot notification)
	menus      = make(map[string][]string)
	pxeHosts   = make(map[string]string)
	insecure   bool
    ipRE = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
)

const (
	dateLayout = "2006-01-02"
	logLayout  = "2006-01-02 15:04:05.999"
	timeLayout = "2006-01-02 15:04:05"
)

//2017-01-31 12:10:03-08:00

func strText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func makeActive(mac string, active bool) {
	aLock.Lock()
	if active {
		activeMACs[mac] = struct{}{}
	} else {
		delete(activeMACs, mac)
	}
	aLock.Unlock()
}

func isActive(mac string) bool {
	aLock.Lock()
	_, ok := activeMACs[mac]
	aLock.Unlock()
	return ok
}

func activeList() []string {
	aLock.Lock()
    a := make([]string,0,len(activeMACs))
    for k, _ := range activeMACs {
        a = append(a, k)
    }
	aLock.Unlock()
	return a
}

func absExecPath() (name string, err error) {
	name = os.Args[0]

	if name[0] == '.' {
		name, err = filepath.Abs(name)
		if err == nil {
			name = filepath.Clean(name)
		}
	} else {
		name, err = exec.LookPath(filepath.Clean(name))
	}
	name = filepath.Dir(name)
	return
}

func pxeExec(site, hostname, ipmi, image string) {
	log.Println("***** PXE EXEC HOST:", ipmi)
	host, ok := pxeHosts[site]
	if !ok {
		log.Printf("host not found for site:%s\n", site)
		return
	}
    mac := getMacHost(ipmi)
    if len(mac) == 0 {
        var err error
        mac, err = findMAC(ipmi)
        if err != nil {
            fmt.Println("findMAC IPMI:", ipmi, "ERR:", err)
        } else {
            setMacHost(mac, hostname)
        }
    }
    makeActive(mac, true)
	log.Printf("pxeboot site:%s ssh:%s impi:%s image:%s\n", site, host, ipmi, image)
    cmd := "sudo /usr/local/bin/pxeboot -f " + ipmi + " " + image 
	out, err := sshcmd(host, cmd, sshTimeout)
	if err != nil {
		log.Println("pxeboot error:", err)
	}
	log.Println("pxeboot out:", out)
}

func init() {

	//filepath.Glob("config.*")
	viper.SetConfigName("config") // name of config file (without extension)
	viper.AddConfigPath(".")
	viper.SetConfigType("toml")
	viper.SetDefault("main.port", 80)
	viper.SetDefault("main.udp-port", 9999)
	//viper.SetDefault("main.dcman", "http://10.100.182.16:8080/dcman/api/site/")
	viper.SetDefault("main.dcman", "http://localhost:8080/dcman/")
	err := viper.ReadInConfig() // Find and read the config file
	if err != nil {             // Handle errors reading the config file
		panic(fmt.Errorf("Fatal error config file: %s \n", err))
	}
	httpPort = viper.GetInt("main.port")
	dcmanURL = viper.GetString("main.dcman")
	dcmanKey = viper.GetString("main.apiKey")
	sshUser = viper.GetString("main.sshUser")
	sshKeyFile = viper.GetString("main.sshKeyFile")
	fmt.Println("************* KEY FILE:", sshKeyFile)
	fmt.Println("************* PORT:", httpPort)
	udpPort = viper.GetInt("main.udp-port")
	fmt.Println("UDP PORT:", udpPort)
	samlURL = viper.GetString("saml.samlURL")
	loginURL = viper.GetString("saml.loginURL")
	authCookie = viper.GetString("saml.cookie")

	key := viper.GetString("main.key")
	if len(key) == 0 {
		key, _ = secrets.KeyGen()
	}
	secrets.SetKey(key)

	if timeout := viper.GetInt("saml.timeout"); timeout > 0 {
		sessionMinutes = time.Duration(timeout) * time.Minute
	}

	data, err := ioutil.ReadFile(sshKeyFile)
	if err != nil {
		panic(err)
	}
	sshKey = string(data)
}

/*
func Map(vs []string, f func(string) string) []string {
	vsm := make([]string, len(vs))
	for i, v := range vs {
		vsm[i] = f(v)
	}
	return vsm
}

func Filter(vs []string, f func(string) string) []string {
	vsm := make([]string, 0, len(vs))
	for i, v := range vs {
		vsm = append([i] = f(v)
	}
	return vsm
}
*/

func menuList(sitename string) ([]string, error) {
	//fmt.Println("menu for:", sitename, "out of ", len(pxeHosts))
	host, ok := pxeHosts[sitename]
	if !ok {
		log.Printf("host not found for site:%s\n", sitename)
		return nil, fmt.Errorf("host not found for site:%s", sitename)
	}
	return getMenus(host), nil
}

func getMenus(hostname string) []string {
	//var menus []string
	//var ok bool
	pLock.Lock()
	m, ok := menus[hostname]
	pLock.Unlock()
	if ok && len(m) > 0 {
		return m
	}
    return refreshMenus(hostname)
}

func refreshMenus(hostname string) []string {
    fmt.Println("refreshing menus for host:", hostname)
	cmd := "pxemenu -m"
	out, err := sshcmd(hostname, cmd, sshTimeout)
	if err != nil {
		log.Println("menu error:", err)
	}
	list := strings.Split(out, "\n")
	n := make([]string, 0, len(list))
	for _, m := range list {
		m = strings.TrimSpace(m)
		if len(m) > 0 && !strings.HasSuffix(m, ".bak") {
			n = append(n, m)
		}
	}
	pLock.Lock()
	menus[hostname] = n
	pLock.Unlock()
	return n
}

func getMacHost(mac string) string {
	macLock.Lock()
	h, _ := macHosts[mac]
	macLock.Unlock()
	return h
}

func setMacHost(mac, hostname string) {
	macLock.Lock()
	macHosts[mac] = hostname
	macLock.Unlock()
}

func addEvent(e event) {
	if err := dbAdd(&e); err != nil {
		fmt.Println("DB ADD ERR:", err)
	}
	sockEcho(e)
}

func sshcmd(hostname, cmd string, timeout int) (string, error) {
	log.Println("SSH HOST:", hostname, "USER:", sshUser, "CMD:", cmd)
	rc, stdout, stderr, err := ssh.ExecText(hostname, sshUser, sshKey, cmd, timeout) //(rc int, stdout, stderr string, err error) {
	if err != nil {
		log.Println("ERR:", err, "STDOUT:", stdout, "STDERR:", stderr)
		return "", err
	}
	if rc != 0 {
		return stderr, fmt.Errorf("cmd exited with error code: %d", rc)
	}
	return stdout, nil
}

func myIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !strings.HasPrefix(ipnet.String(), "127.") && strings.Index(ipnet.String(), ":") == -1 {
			return strings.Split(ipnet.String(), "/")[0]
		}
	}
	return ""
}
func auditLog(uid int64, ip, action, msg string) {
	//log.Println("IP:", ip)
	dbExec("insert into audit_log (uid,ip,action,msg) values(?,?,?,?)", uid, ip, strings.ToLower(action), msg)
}

func init() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	go func() {
		for sig := range c {
			log.Println("Got signal:", sig)
			// sig is a ^C, handle it
			if err := dbExec("PRAGMA wal_checkpoint(FULL)"); err != nil {
				log.Println("checkpoint error:", err)
			}
			if err := dbClose(); err != nil {
				log.Println("close error:", err)
			} else {
				log.Println("db closed")
			}
			os.Exit(1)
		}
	}()

	f, err := os.OpenFile("debug.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	//defer f.Close()

	log.SetOutput(f)
}

//func getSites() (interface{}, error) {
func getSites() ([]site, error) {
	client := &http.Client{}

	fmt.Println("URL:", dcmanURL)
	req, err := http.NewRequest("GET", dcmanURL+"api/site/", nil)
	if err != nil {
		log.Fatal("REQ ERR:", err)
	}
	req.Header.Add("X-API-KEY", dcmanKey)
	resp, err := client.Do(req)
	if err != nil {
		log.Println("sites err:", err)
		return nil, err
	}

	var sites []site
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&sites); err != nil && err != io.EOF {
		return nil, err
	}
	return sites, nil
}

func loadPXE() {
	url := dcmanURL + "/servers"
	client := &http.Client{}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Fatal("REQ ERR:", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatal("DO ERR:", err)
	}
	r := bufio.NewReader(resp.Body)
	cnt := 0
	fields := 0
	var hostIndex, macIndex int
	for {
		cnt++
		if s, err := r.ReadString('\n'); err != nil {
			if err == io.EOF {
				break
			}
			break
		} else {
			s = strings.TrimRight(s, "\n")
			f := strings.Split(s, "\t")
			if cnt == 1 {
				fields = len(f)
				for i, col := range f {
					switch col {
					case "hostname":
						hostIndex = i
					case "mac":
						macIndex = i
					}
				}
			} else {
				if len(f) == fields {
					mac := f[macIndex]
					host := f[hostIndex]
					if len(mac) > 0 && len(host) > 0 {
						//fmt.Println("M:", mac, "H:", host)
						macHosts[mac] = host
					}
				}
			}
		}
	}
}

func getHost(hostname string, sti int64) (interface{}, error) {
	url := dcmanURL + "api/device/pxe/"
	log.Println("get host url:", url)
	client := &http.Client{}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Fatal("REQ ERR:", err)
	}
	// ...
	req.Header.Add("X-API-KEY", dcmanKey)
	q := req.URL.Query()
	//q.Add("debug", "true")
	q.Add("hostname", hostname)
	q.Add("sti", fmt.Sprintf("%d", sti))
	req.URL.RawQuery = q.Encode()
	resp, err := client.Do(req)

	if err != nil {
		return nil, err
	}
	var device interface{}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&device); err != nil && err != io.EOF {
		return nil, err
	}
	return device, nil
}

//
// process expects string with "TS IP WHAT MAC MSG"
//
func process(s string) {
	log.Println("UDP:", s)
	if strings.Contains(s, "HTTP") {
		fmt.Println("\nHTTP:", s)
	}
	fields := strings.Fields(s)
	if len(fields) < 3 {
		fmt.Printf("Not enough fields: %s, (%s)\n", s, fields)
		return
	}
	var ts time.Time
	if len(fields[0]) < 5 {
		ts = time.Now()
	} else {
		seconds, _ := strconv.ParseInt(fields[0], 0, 64)
		ts = time.Unix(seconds, 0)
		before := time.Now().Add(-24 * time.Hour)
		if ts.Before(before) {
			fmt.Println("skipping record, too far back in time:", ts)
			return
		}
	}
	ip := fields[1]
	//var dhcp, mac, hostname, kind, msg string
	var mac, hostname, kind, msg string
	switch fields[2] {
	case "DHCPDISCOVER":
		// DHCP is chatty -- only log info for servers actively being reimaged
		kind = "dhcp"
		msg = "discover"
		mac = fields[3]
		if !isActive(mac) {
			return
		}
		hostname = getMacHost(mac)
		if len(hostname) == 0 {
            fmt.Println("can't find hostname for MAC:", mac)
			return
		}
		//fmt.Printf("TS:%v DISCOVER MAC:%s\n", ts, mac)
	case "DHCPOFFER":
		kind = "dhcp"
		msg = "offer "
		mac = fields[3]
		if !isActive(mac) {
			return
		}
		//dhcp = fields[4]
		hostname = getMacHost(mac)
		if len(hostname) == 0 {
            fmt.Println("can't find hostname for mac:", mac)
			return
		}
		//fmt.Printf("TS:%v OFFER MAC:%s IP:%s\n", ts, mac, dhcp)
	case "HTTP":
		fmt.Println("HTTP HIT:", fields)
		// 1480966729 10.110.192.11 HTTP 10.110.63.227 centos7-platform9.cfg
		kind = "http"
		hostname = getMacHost(fields[3])
		msg = "kickstart: " + strings.Join(fields[4:], " ")
	case "IPMI":
		// echo "- $HOST IPMI $CMD ${MSG[*]}" > /dev/udp/$IMGMAN_HOST/$IMGMAN_PORT
		fmt.Println("IPMI HIT:", fields)
		kind = "ipmi"
		hostname = fields[1]
		msg = "ipmi command: " + strings.Join(fields[3:], " ")
		makeActive(fields[3], true)
	case "PXEFILE":
		kind = "tftp"
		hostname = getMacHost(fields[3])
		if len(fields) > 4 {
			msg = "File: " + fields[4]
		} else {
			msg = "File: " + fields[3]
		}
	case "SHUTDOWN":
		// 2017/01/31 15:30:22 UDP: - - SHUTDOWN 0c:c4:7a:43:0e:52
		kind = "shutdown"
		hostname = getMacHost(fields[3])
		//fmt.Println("SHUTDOWN HOST:",hostname)
		if len(fields) > 4 {
			msg = strings.Join(fields[4:], " ")
		}
	case "BOOT":
		kind = "boot"
		hostname = getMacHost(fields[3])
		//fmt.Println("BOOT HOST:",hostname)
		if len(fields) > 4 {
			msg = strings.Join(fields[4:], " ")
		}
		makeActive(fields[3], false)
	default:
		fmt.Printf("TS:%v IP:%s MSG:%s\n", ts, ip, strings.Join(fields[2:], " "))
		hostname = ip
		msg = strings.Join(fields[2:], " ")
	}
	//fmt.Println("ADD EVNT:", event{TS: ts, Host: hostname, Kind: kind, Msg: msg})
	//addEvent(event{TS: jsonTime(ts), Host: hostname, Kind: kind, Msg: msg})
	addEvent(event{TS: ts, Host: hostname, Kind: kind, Msg: msg})
}

func ipmiHostSave(d pxeDevice) {
    iLock.Lock()
    ipmiHosts[*d.IPMI] = *d.Hostname
    iLock.Unlock()
}

func ipmiHostGet(ipmi string) string {
    if ! ipRE.Match([]byte(ipmi)) {
        return ipmi
    }
    iLock.Lock()
    host, _ := ipmiHosts[ipmi] 
    iLock.Unlock()
    return host
}

func main() {
	var err error
	if err = dbOpen(dbFile+"?_loc=auto", true); err != nil {
		panic(err)
	}

/*
    t1 := tsTest {
	Host: "myhost",
	Msg: "mymsg",
	TS:   time.Now().Unix(),
    }
    if err := dbAdd(&t1); err != nil {
        fmt.Println("ADD ERR:", err)
    }
    dbClose()
    return
*/

	h, err := dbList(&pxeHost{})
	if err != nil {
		panic(err)
	}
	for _, host := range h.([]pxeHost) {
		pxeHosts[strText(host.Sitename)] = strText(host.Hostname)
	}
	loadPXE()
	go udpServer(udpPort)
	go udpReader(udpChan, closer, process)
	fmt.Println("start web with port:", httpPort)
	webServer(httpPort, webHandlers)
}
