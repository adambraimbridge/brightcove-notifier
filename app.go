package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Financial-Times/transactionid-utils-go"
	"github.com/gorilla/mux"
	"github.com/jawher/mow.cli"
	"github.com/pborman/uuid"
)

const logPattern = log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile | log.LUTC

var infoLogger *log.Logger
var warnLogger *log.Logger
var errorLogger *log.Logger

func init() {
	initLogs(os.Stdout, os.Stdout, os.Stderr)
}

type brightcoveNotifier struct {
	port            int
	brightcoveConf  *brightcoveConfig
	cmsNotifierConf *cmsNotifierConfig
	client          *http.Client
}

type brightcoveConfig struct {
	addr        string
	accessToken string
	accountID   string

	//Brightcove OAuth API access token endpoint
	oauthAddr string
	auth      string
}

type cmsNotifierConfig struct {
	addr       string
	auth       string
	hostHeader string
}

func main() {
	app := cli.App("brightcove-notifier", "Gets notified about Brightcove FT video events, creates UPP publish event and posts it to CMS Notifier.")
	port := app.Int(cli.IntOpt{
		Name:   "port",
		Value:  8080,
		Desc:   "application port",
		EnvVar: "PORT",
	})
	brightcove := app.String(cli.StringOpt{
		Name: "brightcove",
		// https://cms.api.brightcove.com/v1/accounts/:account_id/videos/:video_id
		Value:  "https://cms.api.brightcove.com/v1/accounts/",
		Desc:   "brightcove video api address",
		EnvVar: "BRIGHTCOVE",
	})
	brightcoveOAuth := app.String(cli.StringOpt{
		Name:   "brightcove-oauth",
		Value:  "https://oauth.brightcove.com/v3/access_token",
		Desc:   "brightcove oauth api address",
		EnvVar: "BRIGHTCOVE_OAUTH",
	})
	brightcoveAuth := app.String(cli.StringOpt{
		Name: "brightcove-auth",
		// base64encoded value of 'clientId:clientSecret'
		// e.g. "Basic Y2xpZW50SWQ6Y2xpZW50U2VjcmV0"
		Value:  "",
		Desc:   "brightcove oauth api authorization header",
		EnvVar: "BRIGHTCOVE_AUTH",
	})
	brightcoveAccID := app.String(cli.StringOpt{
		Name:   "brightcove-account-id",
		Value:  "",
		Desc:   "brightcove account id: the account with the video events this app gets notified",
		EnvVar: "BRIGHTCOVE_ACCOUNT_ID",
	})
	cmsNotifier := app.String(cli.StringOpt{
		Name:   "cms-notifier",
		Value:  "http://localhost:13080",
		Desc:   "cms notifier address",
		EnvVar: "CMS_NOTIFIER",
	})
	cmsNotifierAuth := app.String(cli.StringOpt{
		Name:   "cms-notifier-auth",
		Value:  "",
		Desc:   "cms notifier authorization header",
		EnvVar: "CMS_NOTIFIER_AUTH",
	})
	cmsNotifierHostHeader := app.String(cli.StringOpt{
		Name:   "cms-notifier-host-header",
		Value:  "",
		Desc:   "cms notifier host header",
		EnvVar: "CMS_NOTIFIER_HOST_HEADER",
	})

	app.Action = func() {
		bn := &brightcoveNotifier{
			port: *port,
			brightcoveConf: &brightcoveConfig{
				addr:      *brightcove,
				oauthAddr: *brightcoveOAuth,
				auth:      *brightcoveAuth,
				accountID: *brightcoveAccID,
			},
			cmsNotifierConf: &cmsNotifierConfig{
				addr:       *cmsNotifier,
				auth:       *cmsNotifierAuth,
				hostHeader: *cmsNotifierHostHeader,
			},
			client: &http.Client{},
		}
		infoLogger.Println(bn.prettyPrint())
		go bn.listen()
		ch := make(chan os.Signal)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		infoLogger.Println("Received termination signal. Quitting... \nBye")
	}
	err := app.Run(os.Args)
	if err != nil {
		errorLogger.Printf("[%v]", err)
	}
}

func (bn brightcoveNotifier) listen() {
	r := mux.NewRouter()
	r.HandleFunc("/notify", bn.handleNotification).Methods("POST")
	r.HandleFunc("/force-notify/{id}", bn.handleForceNotification).Methods("POST")
	r.HandleFunc("/__health", bn.health()).Methods("GET")
	r.HandleFunc("/__gtg", bn.gtg).Methods("GET")

	http.Handle("/", r)
	infoLogger.Printf("Starting to listen on port [%d]", bn.port)
	err := http.ListenAndServe(":"+strconv.Itoa(bn.port), nil)
	if err != nil {
		errorLogger.Panicf("Couldn't set up HTTP listener: %+v\n", err)
	}
}

type videoEvent struct {
	TimeStamp int64  `json:"timestamp"`
	AccountID string `json:"account_id"`
	Event     string `json:"event"`
	Video     string `json:"video"`
	Version   int    `json:"version"`
}

func (ve videoEvent) String() string {
	return fmt.Sprintf("videoEvent: TimeStamp: [%s], AccountId: [%s], Event: [%s], Video: [%s], Version: [%d]",
		time.Unix(0, ve.TimeStamp*int64(time.Millisecond)).Format(time.RFC3339), ve.AccountID, ve.Event, ve.Video, ve.Version)
}

func (bn brightcoveNotifier) handleForceNotification(w http.ResponseWriter, r *http.Request) {
	transactionID := transactionidutils.GetTransactionIDFromRequest(r)
	video, err := bn.fetchVideo(videoEvent{Video: mux.Vars(r)["id"]}, transactionID)
	if err != nil {
		warnLogger.Printf("tid=%v video_id=%v Fetching video unsuccessful: %v", transactionID, mux.Vars(r)["id"], err)
		if err.Error() == "Too many requests. status=429" {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if video["error_code"] == "NOT_FOUND" {
		infoLogger.Printf("tid=%v video_id=%s Video was not found in Brightcove API.", transactionID, video["id"])
	} else {
		infoLogger.Printf("tid=%v video_id=%s Fetching video successful.", transactionID, video["id"])
	}

	err = addUPPRequiredFields(video)
	if err != nil {
		warnLogger.Printf("tid=[%v]. [%v]", transactionID, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	infoLogger.Printf("tid=%v video_id=%v uuid=%v Generated uuid for video.", transactionID, video["id"], video["uuid"])
	err = bn.fwdVideo(video, transactionID)
	if err != nil {
		warnLogger.Printf("tid=%v video_id=%v Forwarding video unsuccessful.", transactionID, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	infoLogger.Printf("tid=%v video_id=%s Forwarding video successful.", transactionID, video["id"])
	if video["error_code"] == "NOT_FOUND" {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (bn brightcoveNotifier) handleNotification(w http.ResponseWriter, r *http.Request) {
	transactionID := transactionidutils.GetTransactionIDFromRequest(r)

	var event videoEvent
	err := json.NewDecoder(r.Body).Decode(&event)
	if err != nil {
		warnLogger.Printf("tid=%v Invalid request received: %v", transactionID, err)
		return
	}

	if bn.brightcoveConf.accountID != event.AccountID {
		warnLogger.Printf("tid=%v account_id=%v Invalid notification event received. Unexpected accountID. Ignoring...", transactionID, event.AccountID)
		return
	}
	infoLogger.Printf("tid=%v video_id=%v Received notification event for video.", transactionID, event.Video)

	video, err := bn.fetchVideo(event, transactionID)
	if err != nil {
		warnLogger.Printf("tid=[%v]. Fetching video: [%v]", transactionID, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	infoLogger.Printf("tid=%v video_id=%s Fetching video successful.", transactionID, video["id"])

	err = addUPPRequiredFields(video)
	if err != nil {
		warnLogger.Printf("tid=%v %v", transactionID, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	infoLogger.Printf("tid=%v video_id=%v uuid=%v Generated uuid for video.", transactionID, video["id"], video["uuid"])

	err = bn.fwdVideo(video, transactionID)
	if err != nil {
		warnLogger.Printf("tid=%v video_id=%s Forwarding video unsuccessful: [%v]", transactionID, video["id"], err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	infoLogger.Printf("tid=%v video_id=%s Forwarding video successful.", transactionID, video["id"])
}

func addUPPRequiredFields(video video) error {
	//generate uuid
	id, ok := video["id"].(string)
	if !ok {
		return fmt.Errorf("Invalid content, missing video ID.")
	}
	video["uuid"] = uuid.NewMD5(uuid.UUID{}, []byte(id)).String()

	video["type"] = "video"
	return nil
}

type video map[string]interface{}

func (bn brightcoveNotifier) fetchVideo(ve videoEvent, tid string) (video, error) {
	req, err := http.NewRequest("GET", bn.brightcoveConf.addr+bn.brightcoveConf.accountID+"/videos/"+ve.Video, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-type", "application/json")
	req.Header.Add("Authorization", "Bearer "+bn.brightcoveConf.accessToken)
	resp, err := bn.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer cleanupResp(resp)
	switch resp.StatusCode {
	case 401:
		infoLogger.Printf("tid=[%s]. Renewing access token.", tid)
		err = bn.renewAccessToken()
		if err != nil {
			e := fmt.Errorf("Renewing access token failure: [%v].", err)
			return nil, e
		}
		return bn.fetchVideo(ve, tid)
	case 404:
		var notFound []map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&notFound)
		if err != nil {
			return nil, err
		}
		if len(notFound) == 0 {
			return nil, fmt.Errorf("Unexpected 404 response. Zero-length array received.")
		}
		notFound[0]["id"] = ve.Video
		return notFound[0], nil
	case 200:
		var v video
		err = json.NewDecoder(resp.Body).Decode(&v)
		if err != nil {
			return nil, err
		}
		return v, nil
	default:
		return nil, fmt.Errorf("Invalid statusCode received: [%d]", resp.StatusCode)
	}
}

func (bn brightcoveNotifier) fwdVideo(video video, tid string) error {
	videoJSON, err := json.Marshal(video)
	if err != nil {
		return err
	}
	addr := bn.cmsNotifierConf.addr + "/notify"
	req, err := http.NewRequest("POST", addr, bytes.NewReader(videoJSON))
	if err != nil {
		return err
	}
	req.Header.Add("Content-type", "application/json")
	req.Header.Add("X-Origin-System-Id", "brightcove")
	req.Header.Add("X-Request-Id", tid)
	if bn.cmsNotifierConf.auth != "" {
		req.Header.Add("Authorization", bn.cmsNotifierConf.auth)
	}
	if bn.cmsNotifierConf.hostHeader != "" {
		req.Host = bn.cmsNotifierConf.hostHeader
	}
	resp, err := bn.client.Do(req)
	if err != nil {
		return err
	}
	defer cleanupResp(resp)
	switch resp.StatusCode {
	case 400:
		msg, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("Status code 400. [%s]", string(msg[:]))
	case 200:
		return nil
	default:
		return fmt.Errorf("Invalid statusCode received: [%d]", resp.StatusCode)
	}
}

const tokenRequest = "grant_type=client_credentials"

type accessTokenResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Expires     int    `json:"expires_in"`
}

func (bn brightcoveNotifier) renewAccessToken() (err error) {
	req, err := http.NewRequest("POST", bn.brightcoveConf.oauthAddr, bytes.NewReader([]byte(tokenRequest)))
	if err != nil {
		return err
	}
	req.Header.Add("Content-type", "application/x-www-form-urlencoded")
	req.Header.Add("Authorization", bn.brightcoveConf.auth)
	resp, err := bn.client.Do(req)
	if err != nil {
		return err
	}
	defer cleanupResp(resp)
	if resp.StatusCode != 200 {
		return fmt.Errorf("Invalid statusCode received: [%d]", resp.StatusCode)
	}
	var accTokenResp accessTokenResp
	err = json.NewDecoder(resp.Body).Decode(&accTokenResp)
	if err != nil {
		return err
	}
	if accTokenResp.AccessToken == "" {
		return fmt.Errorf("Empty access token: [%#v]", accTokenResp)
	}
	bn.brightcoveConf.accessToken = accTokenResp.AccessToken
	return nil
}

func cleanupResp(resp *http.Response) {
	_, err := io.Copy(ioutil.Discard, resp.Body)
	if err != nil {
		warnLogger.Printf("[%v]", err)
	}
	err = resp.Body.Close()
	if err != nil {
		warnLogger.Printf("[%v]", err)
	}
}

func initLogs(infoHandle io.Writer, warnHandle io.Writer, errorHandle io.Writer) {
	infoLogger = log.New(infoHandle, "INFO  - ", logPattern)
	warnLogger = log.New(warnHandle, "WARN  - ", logPattern)
	errorLogger = log.New(errorHandle, "ERROR - ", logPattern)
}

func (bn brightcoveNotifier) prettyPrint() string {
	return fmt.Sprintf("Config: [\n\tport: [%d]\n\tbrightcoveConf: [%s]\n\tcmsNotifierConf: [%s]\n]", bn.port, bn.brightcoveConf.prettyPrint(), bn.cmsNotifierConf.prettyPrint())
}

func (bc brightcoveConfig) prettyPrint() string {
	authSet := "empty"
	if bc.auth != "" {
		authSet = "set, not empty"
	}
	accessTokenSet := "empty"
	if bc.accessToken != "" {
		accessTokenSet = "set, not empty"
	}
	return fmt.Sprintf("\n\t\taddr: [%s]\n\t\toauthAddr: [%s]\n\t\taccountID: [%s]\n\t\tauth: [%s]\n\t\taccessToken: [%s]\n\t", bc.addr, bc.oauthAddr, bc.accountID, authSet, accessTokenSet)
}

func (cnc cmsNotifierConfig) prettyPrint() string {
	authSet := "empty"
	if cnc.auth != "" {
		authSet = "set, not empty"
	}
	return fmt.Sprintf("\n\t\taddr: [%s]\n\t\thostHeader: [%s]\n\t\tauth: [%s]\n\t", cnc.addr, cnc.hostHeader, authSet)
}
