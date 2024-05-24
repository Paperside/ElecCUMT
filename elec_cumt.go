package main

import (
	// "fmt"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"syscall"

	// "os/exec"
	"strconv"
	"strings"

	// "go/build"
	// "errors"
	"bytes"
	"io"
	"net/http"
	"os"
	"os/signal"

	log "github.com/sirupsen/logrus"
	"gopkg.in/gomail.v2"
	"gopkg.in/natefinch/lumberjack.v2"

	// "regexp"
	"time"
)

/* Templates */

const configFilePath string = "elec_cumt_config.json"

var logFile *lumberjack.Logger
var config Config
var dormstatus = make(map[string]DormRemindStatus)
var history = make(map[string][10]Elecinfo)

type Dormitory struct {
	Roomid   string `json:"roomid"`
	Building string `json:"building"`
}
type DormRemindStatus struct {
	Sent50 bool
	Sent20 bool
	Sent10 bool
}
type Elecinfo struct {
	Rawinfo string
	Roomid  string
	Elec    float64
	Time    time.Time
}

type Config struct {
	LogFilePath   string `json:"log_file_path"`
	CacheFilePath string `json:"cache_file_path"`
	HTMLFilePath  string `json:"html_file_path"`
	FetchUrl      string `json:"fetch_url"`

	Dormitory []Dormitory `json:"dormitory"`

	SmtpServer   string              `json:"smtp_server"`
	SmtpPort     int                 `json:"smtp_port"`
	SmtpUsername string              `json:"smtp_username"`
	SmtpPassword string              `json:"smtp_password"`
	EmailAdmin   string              `json:"email_admin"`
	EmailList    map[string][]string `json:"email_list"`
}

type Cache struct {
	CacheDormStatus map[string]DormRemindStatus
	CacheHistory    map[string][10]Elecinfo
}

/* Components */

func readConfig() (Config, error) {
	file, err := os.ReadFile(configFilePath)
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(file, &config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func loadCache() error {
	_, cacheerr := os.Stat(config.CacheFilePath + "cache.json")
	if cacheerr != nil {
		return errors.New("no such file or directory:" + config.CacheFilePath + "cache.json" + ", no cache data loaded.")
	}
	log.Info("Cached data founded in ", config.CacheFilePath+"cache.json", " ,trying to load data from cache file...")
	cacheFile, err := os.ReadFile(config.CacheFilePath + "cache.json")
	if err != nil {
		return errors.New("failed to read cache file, giving up...")
	}
	var cache Cache
	if err := json.Unmarshal(cacheFile, &cache); err != nil {
		return errors.New("failed to parse cache file, giving up...")
	}
	for _, info := range config.Dormitory {
		if _, ok := cache.CacheDormStatus[info.Roomid]; ok {
			dormstatus[info.Roomid] = cache.CacheDormStatus[info.Roomid]
		}
		if _, ok := cache.CacheHistory[info.Roomid]; ok {
			history[info.Roomid] = cache.CacheHistory[info.Roomid]
		}
	}
	return nil
}

func fetchElecInfo() ([]Elecinfo, error) {
	var elecinfos []Elecinfo
	for index, dormitory := range config.Dormitory {
		log.Info("Fetching Elec Info: ", index+1, "/", len(config.Dormitory), "......")
		// Send Request
		data := new(bytes.Buffer)
		fmt.Fprint(data, `jsondata={ "query_elec_roominfo": { "aid":"0030000000002501", "account": "138791","room": { "roomid": "`+dormitory.Roomid+`", "room": "" },  "floor": { "floorid": "", "floor": "" }, "area": { "area": "1", "areaname": "" }, "building": { "buildingid": "`+dormitory.Building+`", "building": "" } } }&funname=synjones.onecard.query.elec.roominfo&json=true`)
		response, err := http.Post(config.FetchUrl, "application/x-www-form-urlencoded; charset=UTF-8", data)
		if err != nil {
			return nil, errors.New("Failed to send request: " + err.Error())
		}
		defer response.Body.Close()

		// read response and decode to json data
		responseData, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, errors.New("Failed to read response data: " + err.Error())
		}
		var res interface{}
		if err := json.NewDecoder(bytes.NewReader(responseData)).Decode(&res); err != nil {
			return nil, errors.New("Failed to decode response data: " + err.Error())
		}

		// parse elec info
		var elecinfo Elecinfo
		elecinfo.Time = time.Now()
		if raw, ok := res.(map[string]interface{})["query_elec_roominfo"].(map[string]interface{})["errmsg"].(string); ok {
			elecinfo.Rawinfo = raw
			//the rawinfo is expected to be " T4A612房间剩余电量718.01", we need to exract"T4A612" and "718.01" to elecinfo.Roomid and elecinfo.Elec
			start := strings.Index(raw, "房间剩余电量")
			if start != -1 {
				elecinfo.Roomid = raw[1:start]
				temp, err := strconv.ParseFloat(raw[start+len("房间剩余电量"):], 64)
				if err != nil {
					return nil, errors.New("Message parse succeeded, but failed to parse elec info: " + err.Error())
				}
				elecinfo.Elec = temp
			} else {
				return nil, errors.New("Message parse succeeded, but failed to parse elec info: " + raw)
			}
		} else {
			return nil, errors.New("Failed to parse message: " + fmt.Sprint(res))
		}
		elecinfos = append(elecinfos, elecinfo)
	}
	return elecinfos, nil
}

func generateElecEmailBody(elecinfo Elecinfo, htmpath string) (string, error) {
	file, err := os.ReadFile(htmpath)
	if err != nil {
		return "", err
	}
	var elecrecord string = "<table class=\"table\"><thead><tr><th class=\"th\">房间号</th><th class=\"th\">查询时间</th><th class=\"th\">剩余电量 (kWh)</th></tr></thead><tbody>"
	for i := len(history[elecinfo.Roomid]) - 1; i >= 0; i-- {
		if history[elecinfo.Roomid][i].Rawinfo == "" {
			break
		}
		elecrecord += "<tr><td class=\"th\">" + history[elecinfo.Roomid][i].Roomid + "</td>" + "<td class=\"th\">" + history[elecinfo.Roomid][i].Time.Format("2006-01-02 15:04:05") + "</td>" + "<td class=\"th\">" + fmt.Sprint(history[elecinfo.Roomid][i].Elec) + "</td></tr>"
	}
	elecrecord += "</tbody></table>"
	body := string(file)
	//replace all "@slot_roomname" with elecinfo.Roomid and "@slot_elec" with elecinfo.Elec and "@slot_querytime" with elecinfo.Time
	body = strings.ReplaceAll(body, "@slot_roomname", elecinfo.Roomid)
	body = strings.ReplaceAll(body, "@slot_elec", fmt.Sprintf("%.2f", elecinfo.Elec))
	body = strings.ReplaceAll(body, "@slot_querytime", elecinfo.Time.Format("2006-01-02 15:04:05"))
	body = strings.ReplaceAll(body, "@slot_Elecrecord", elecrecord)
	return body, nil
}

func sendEmail(subject string, body string, to []string) error {
	m := gomail.NewMessage()
	m.SetHeader("From", config.SmtpUsername)
	m.SetHeader("To", to...)
	m.SetHeader("Subject", subject)
	m.SetBody("text/html", body)

	d := gomail.NewDialer(config.SmtpServer, config.SmtpPort, config.SmtpUsername, config.SmtpPassword)
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	if err := d.DialAndSend(m); err != nil {
		return err
	}
	return nil
}

func sendLackofElecEmail(elecinfo Elecinfo) error {
	emailbody, err := generateElecEmailBody(elecinfo, config.HTMLFilePath+"notification.html")
	if err != nil {
		return errors.New("Failed to generate email body: " + err.Error())
	}
	for _, email := range config.EmailList[elecinfo.Roomid] {
		err = sendEmail("电量提醒", emailbody, []string{email})
		if err != nil {
			log.Error("Dorm:", elecinfo.Roomid, " Email:", email, " Status:", "Failed to send email", " Error:", err)
			return errors.New("Dorm:" + elecinfo.Roomid + " Email:" + email + " Status:" + "Failed to send email" + " Error:" + err.Error())
		}
		log.Info("Dorm:", elecinfo.Roomid, " Email:", email, " Status:", "Email sent successfully")
	}
	return nil
}

func executeElecCheck() error {
	elecinfos, err := fetchElecInfo()
	if err != nil {
		return errors.New("Failed to fetch elec info: " + err.Error())
	}
	for index, elecinfo := range elecinfos {
		log.Info("Traversing:" + fmt.Sprint(index+1, "/", len(elecinfos)) + " Roomid:" + elecinfo.Roomid + " Elec:" + fmt.Sprint(elecinfo.Elec) + " Time:" + fmt.Sprint(elecinfo.Time.Format("2006-01-02 15:04:05")))

		//update history
		var k = len(history[elecinfo.Roomid])
		if k == 10 {
			updatedHistory := make([]Elecinfo, 10)
			previousHistory := history[elecinfo.Roomid]
			copy(updatedHistory[:9], previousHistory[1:]) //slice [start:end] the "end" index will not be included, so there should be [(0):9] [1:(10)], rather tha [0:8] [1:9]
			updatedHistory[9] = elecinfo
			history[elecinfo.Roomid] = [10]Elecinfo(updatedHistory)
		} else {
			log.Error("History updated failed: ", "history length is not 10")
		}

		//if elec lacks, call the func
		if elecinfo.Elec <= 50 {
			log.Info(elecinfo.Roomid, ": Elec lack Detected!")
			if !dormstatus[elecinfo.Roomid].Sent50 {
				dormstatus[elecinfo.Roomid] = DormRemindStatus{true, false, false}
				err := sendLackofElecEmail(elecinfo)
				if err != nil {
					return err
				}
			} else if !dormstatus[elecinfo.Roomid].Sent20 && elecinfo.Elec <= 20 {
				dormstatus[elecinfo.Roomid] = DormRemindStatus{true, true, false}
				err := sendLackofElecEmail(elecinfo)
				if err != nil {
					return err
				}
			} else if !dormstatus[elecinfo.Roomid].Sent10 && elecinfo.Elec <= 10 {
				dormstatus[elecinfo.Roomid] = DormRemindStatus{true, true, true}
				err := sendLackofElecEmail(elecinfo)
				if err != nil {
					return err
				}
			}
		} else {
			log.Info(elecinfo.Roomid, ": Elec rest is adequate: ", elecinfo.Elec, "KWh")
			dormstatus[elecinfo.Roomid] = DormRemindStatus{false, false, false}
		}
	}
	return nil
}

func scheduledElecCheck() {
	for {
		log.Info("Executing Elec Check......")
		err := executeElecCheck()
		if err != nil {
			log.Error("Failed to executing elec check, Error Info: ", err)
			log.Info("App will try to execute elec check every 10 mins because of the failed check above, until at least one successful check is performed...")
			next := time.Now().Add(time.Minute * 10)
			log.Info("Thus the next check will be executed at ", next.Format("2006-01-02 15:04:05"))
			time.Sleep(time.Until(next))
			continue
		}
		now := time.Now()
		next := now.Add(time.Hour * 8)
		log.Info("Elec Check Executed Successfully! Next Check is Scheduled to be Executed in 8 hours at ", next.Format("2006-01-02 15:04:05"))
		duration := next.Sub(now)
		time.Sleep(duration)
	}
}
func onExiting() {
	log.Info("Received exit signal, app will close after trying to save cache data...")
	_, err := os.Stat(config.CacheFilePath)
	if err != nil {
		err := os.MkdirAll(config.CacheFilePath, 0777)
		if err != nil {
			log.Info("No such directory:", config.CacheFilePath, " and failed to create the directory, app will exit directly with no data cached!")
		}
	}
	cachefile, err := os.OpenFile(config.CacheFilePath+"cache.json", os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0777)
	if err != nil {
		log.Info("Failed to create cache file,  app will exit directly with no data cached!")
	}
	defer cachefile.Close()
	var cache Cache = Cache{
		CacheDormStatus: dormstatus,
		CacheHistory:    history,
	}
	cache_json, _ := json.Marshal(cache)
	_, writerr := cachefile.Write(cache_json)
	if writerr != nil {
		log.Info("Failed to write cache file with err:", writerr, " ,app will exit immediately...")
	}
	log.Info("Successfully write cache data into:", config.CacheFilePath, "cache.json, app will exit immediately...")
}

/* Main */

func init() {
	config_t, err := readConfig()
	if err != nil {
		log.SetLevel(log.ErrorLevel)
		log.Fatal("Failed to read config file: ", err)
		return
	}
	config = config_t
	fmt.Println(config)

	logFile = &lumberjack.Logger{
		Filename:   config.LogFilePath + "app.log",
		MaxSize:    10, // megabytes
		MaxBackups: 3,
		MaxAge:     180, //days
	}
	logWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(logWriter)
	log.SetLevel(log.TraceLevel)

	log.Info("App eleccumt initializing...")

	//initialize data
	var template1 DormRemindStatus = DormRemindStatus{false, false, false} //Default initialization is all nil(nil for the specialized types)
	var template2 [10]Elecinfo = [10]Elecinfo{}
	for _, info := range config.Dormitory {
		dormstatus[info.Roomid] = template1
		history[info.Roomid] = template2
	}

	//load cache
	if err := loadCache(); err != nil {
		log.Error("Failed to load cache data:", err)
	} else {
		log.Info("Cache data load successfully!")
		fmt.Println(history, dormstatus)
	}

	log.Info("Init success!")
}

func main() {
	//catch signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go scheduledElecCheck()

	select {
	case <-sigChan:
		onExiting()
	}

}
