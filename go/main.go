package main

import (
	"bytes"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
)

const (
	sessionName                 = "isucondition_go"
	conditionLimit              = 20
	frontendContentsPath        = "../public"
	jiaJWTSigningKeyPath        = "../ec256-public.pem"
	defaultIconFilePath         = "../NoImage.jpg"
	defaultJIAServiceURL        = "http://localhost:5000"
	mysqlErrNumDuplicateEntry   = 1062
	conditionLevelInfo          = "info"
	conditionLevelWarning       = "warning"
	conditionLevelCritical      = "critical"
	scoreConditionLevelInfo     = 3
	scoreConditionLevelWarning  = 2
	scoreConditionLevelCritical = 1
)

var (
	db                  *sqlx.DB
	sessionStore        sessions.Store
	mySQLConnectionData *MySQLConnectionEnv

	jiaJWTSigningKey *ecdsa.PublicKey
	jiaServiceURL    string = ""

	postIsuConditionTargetBaseURL string // JIAへのactivate時に登録する，ISUがconditionを送る先のURL

	isuConditionWorker *IsuConditionWorker
)

type Isu struct {
	ID              int              `db:"id" json:"id"`
	JIAIsuUUID      string           `db:"jia_isu_uuid" json:"jia_isu_uuid"`
	Name            string           `db:"name" json:"name"`
	Character       string           `db:"character" json:"character"`
	JIAUserID       string           `db:"jia_user_id" json:"-"`
	CreatedAt       time.Time        `db:"created_at" json:"-"`
	UpdatedAt       time.Time        `db:"updated_at" json:"-"`
	LatestCondition NullIsuCondition `db:"latest_condition" json:"-"`
}

type IsuFromJIA struct {
	Character string `json:"character"`
}

type GetIsuListResponse struct {
	ID                 int                      `json:"id"`
	JIAIsuUUID         string                   `json:"jia_isu_uuid"`
	Name               string                   `json:"name"`
	Character          string                   `json:"character"`
	LatestIsuCondition *GetIsuConditionResponse `json:"latest_isu_condition"`
}

type NullIsuCondition struct {
	JIAIsuUUID sql.NullString `db:"jia_isu_uuid"`
	Timestamp  sql.NullTime   `db:"timestamp"`
	IsSitting  sql.NullBool   `db:"is_sitting"`
	Condition  sql.NullString `db:"condition"`
	Message    sql.NullString `db:"message"`
}

type IsuCondition struct {
	JIAIsuUUID          string    `db:"jia_isu_uuid"`
	Timestamp           time.Time `db:"timestamp"`
	IsSitting           bool      `db:"is_sitting"`
	Condition           string    `db:"condition"`
	ScoreConditionLevel int       `db:"score_condition_level"`
	Message             string    `db:"message"`
	CreatedAt           time.Time `db:"created_at"`
}

type IsuConditionWorker struct {
	mu                   sync.Mutex
	isuConditions        []*IsuCondition
	cachedTrendResponses []*TrendResponse
}

type MySQLConnectionEnv struct {
	Host     string
	Port     string
	User     string
	DBName   string
	Password string
}

type InitializeRequest struct {
	JIAServiceURL string `json:"jia_service_url"`
}

type InitializeResponse struct {
	Language string `json:"language"`
}

type GetMeResponse struct {
	JIAUserID string `json:"jia_user_id"`
}

type GraphResponse struct {
	StartAt             int64           `json:"start_at"`
	EndAt               int64           `json:"end_at"`
	Data                *GraphDataPoint `json:"data"`
	ConditionTimestamps []int64         `json:"condition_timestamps"`
}

type GraphDataPoint struct {
	Score      int                  `json:"score"`
	Percentage ConditionsPercentage `json:"percentage"`
}

type ConditionsPercentage struct {
	Sitting      int `json:"sitting"`
	IsBroken     int `json:"is_broken"`
	IsDirty      int `json:"is_dirty"`
	IsOverweight int `json:"is_overweight"`
}

type GraphDataPointWithInfo struct {
	JIAIsuUUID          string
	StartAt             time.Time
	Data                GraphDataPoint
	ConditionTimestamps []int64
}

type GetIsuConditionResponse struct {
	JIAIsuUUID     string `json:"jia_isu_uuid"`
	IsuName        string `json:"isu_name"`
	Timestamp      int64  `json:"timestamp"`
	IsSitting      bool   `json:"is_sitting"`
	Condition      string `json:"condition"`
	ConditionLevel string `json:"condition_level"`
	Message        string `json:"message"`
}

type TrendResponse struct {
	Character string            `json:"character"`
	Info      []*TrendCondition `json:"info"`
	Warning   []*TrendCondition `json:"warning"`
	Critical  []*TrendCondition `json:"critical"`
}

type TrendCondition struct {
	ID        int   `json:"isu_id"`
	Timestamp int64 `json:"timestamp"`
}

type PostIsuConditionRequest struct {
	IsSitting bool   `json:"is_sitting"`
	Condition string `json:"condition"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

type JIAServiceRequest struct {
	TargetBaseURL string `json:"target_base_url"`
	IsuUUID       string `json:"isu_uuid"`
}

func getEnv(key string, defaultValue string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}
	return defaultValue
}

func NewMySQLConnectionEnv() *MySQLConnectionEnv {
	return &MySQLConnectionEnv{
		Host:     getEnv("MYSQL_HOST", "127.0.0.1"),
		Port:     getEnv("MYSQL_PORT", "3306"),
		User:     getEnv("MYSQL_USER", "isucon"),
		DBName:   getEnv("MYSQL_DBNAME", "isucondition"),
		Password: getEnv("MYSQL_PASS", "isucon"),
	}
}

func (mc *MySQLConnectionEnv) ConnectDB() (*sqlx.DB, error) {
	dsn := fmt.Sprintf("%v:%v@tcp(%v:%v)/%v?parseTime=true&loc=Asia%%2FTokyo", mc.User, mc.Password, mc.Host, mc.Port, mc.DBName)
	return sqlx.Open("mysql", dsn)
}

func newIsuConditionWorker() *IsuConditionWorker {
	w := &IsuConditionWorker{}
	w.Reset()
	return w
}

func (w *IsuConditionWorker) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.isuConditions = []*IsuCondition{}
	w.cachedTrendResponses = nil
}

func (w *IsuConditionWorker) Start(logger echo.Logger) {
	go func() {
		for {
			time.Sleep(300 * time.Microsecond)

			insertCount, err := w.bulkInsertIsuConditions()
			if err != nil {
				logger.Errorf("%v", err)
			}

			if w.cachedTrendResponses == nil || insertCount > 0 {
				logger.Infof("Inserted `isu_condition` %d rows.", insertCount)
				err := w.updateTrendResponsesCache()
				if err != nil {
					logger.Errorf("%v", err)
				} else {
					logger.Info("Updated cachedTrendResponses Cache")
				}
			}
		}
	}()
}

func (w *IsuConditionWorker) AppendIsuConditions(elms []*IsuCondition) {
	if len(elms) == 0 {
		return
	}
	w.mu.Lock()
	w.isuConditions = append(w.isuConditions, elms...)
	w.mu.Unlock()
}

func (w *IsuConditionWorker) GetTrendResponses() []*TrendResponse {
	var res []*TrendResponse
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, trendResponse := range w.cachedTrendResponses {
		copied := *trendResponse
		res = append(res, &copied)
	}
	return res
}

func (w *IsuConditionWorker) bulkInsertIsuConditions() (int, error) {
	queryArgs := []interface{}{}
	w.mu.Lock()
	insertCount := len(w.isuConditions)
	if insertCount > 10000 {
		insertCount = 10000
	}
	for i := 0; i < insertCount; i++ {
		var isSitting int
		if w.isuConditions[i].IsSitting {
			isSitting = 1
		} else {
			isSitting = 0
		}
		queryArgs = append(
			queryArgs,
			w.isuConditions[i].JIAIsuUUID,
			w.isuConditions[i].Timestamp.Format("2006-01-02 15:04:05"),
			isSitting,
			w.isuConditions[i].Condition,
			w.isuConditions[i].ScoreConditionLevel,
			w.isuConditions[i].Message,
		)
	}
	w.isuConditions = w.isuConditions[insertCount:]
	w.mu.Unlock()
	if insertCount == 0 {
		return 0, nil
	}
	_, err := db.Exec(fmt.Sprintf(
		"INSERT INTO `isu_condition`"+
			"	(`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `score_condition_level`, `message`)"+
			"	VALUES "+
			strings.Repeat("('%s', '%s', %d, '%s', %d, '%s'), ", insertCount-1)+
			"('%s', '%s', %d, '%s', %d, '%s')",
		queryArgs...))
	if err != nil {
		return 0, fmt.Errorf("db error: %v", err)
	}
	return insertCount, nil
}

func (w *IsuConditionWorker) updateTrendResponsesCache() error {
	allIsuList := []Isu{}
	err := db.Select(&allIsuList,
		"SELECT `isu`.*"+
			" ,`latest_condition`.`jia_isu_uuid` \"latest_condition.jia_isu_uuid\""+
			" ,`latest_condition`.`timestamp` \"latest_condition.timestamp\""+
			" ,`latest_condition`.`is_sitting` \"latest_condition.is_sitting\""+
			" ,`latest_condition`.`condition` \"latest_condition.condition\""+
			" ,`latest_condition`.`message` \"latest_condition.message\""+
			" FROM `isu`"+
			" LEFT JOIN `isu_condition` AS `latest_condition`"+
			" ON `latest_condition`.`jia_isu_uuid` = `isu`.`jia_isu_uuid`"+
			" AND `latest_condition`.`timestamp` = (SELECT MAX(`timestamp`) FROM `isu_condition` WHERE `jia_isu_uuid` = `isu`.`jia_isu_uuid`)")
	if err != nil {
		return fmt.Errorf("db error: %v", err)
	}
	isuListByCharacter := map[string][]Isu{}
	for _, isu := range allIsuList {
		if isu.LatestCondition.JIAIsuUUID.Valid {
			isuListByCharacter[isu.Character] = append(isuListByCharacter[isu.Character], isu)
		}
	}

	res := []*TrendResponse{}

	for character, isuList := range isuListByCharacter {
		characterInfoIsuConditions := []*TrendCondition{}
		characterWarningIsuConditions := []*TrendCondition{}
		characterCriticalIsuConditions := []*TrendCondition{}
		for _, isu := range isuList {
			conditionLevel, err := calculateScoreConditionLevel(isu.LatestCondition.Condition.String)
			if err != nil {
				return fmt.Errorf("db error: %v", err)
			}
			trendCondition := TrendCondition{
				ID:        isu.ID,
				Timestamp: isu.LatestCondition.Timestamp.Time.Unix(),
			}
			switch conditionLevel {
			case scoreConditionLevelInfo:
				characterInfoIsuConditions = append(characterInfoIsuConditions, &trendCondition)
			case scoreConditionLevelWarning:
				characterWarningIsuConditions = append(characterWarningIsuConditions, &trendCondition)
			case scoreConditionLevelCritical:
				characterCriticalIsuConditions = append(characterCriticalIsuConditions, &trendCondition)
			}
		}

		sort.Slice(characterInfoIsuConditions, func(i, j int) bool {
			return characterInfoIsuConditions[i].Timestamp > characterInfoIsuConditions[j].Timestamp
		})
		sort.Slice(characterWarningIsuConditions, func(i, j int) bool {
			return characterWarningIsuConditions[i].Timestamp > characterWarningIsuConditions[j].Timestamp
		})
		sort.Slice(characterCriticalIsuConditions, func(i, j int) bool {
			return characterCriticalIsuConditions[i].Timestamp > characterCriticalIsuConditions[j].Timestamp
		})
		res = append(res,
			&TrendResponse{
				Character: character,
				Info:      characterInfoIsuConditions,
				Warning:   characterWarningIsuConditions,
				Critical:  characterCriticalIsuConditions,
			})
	}

	w.mu.Lock()
	w.cachedTrendResponses = res
	w.mu.Unlock()

	return nil
}

func init() {
	sessionStore = sessions.NewCookieStore([]byte(getEnv("SESSION_KEY", "isucondition")))

	key, err := ioutil.ReadFile(jiaJWTSigningKeyPath)
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}
	jiaJWTSigningKey, err = jwt.ParseECPublicKeyFromPEM(key)
	if err != nil {
		log.Fatalf("failed to parse ECDSA public key: %v", err)
	}
}

func main() {
	e := echo.New()
	e.Debug = true
	e.Logger.SetLevel(log.DEBUG)

	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.POST("/initialize", postInitialize)
	e.POST("/reset-worker", postResetWorker)

	e.POST("/api/auth", postAuthentication)
	e.POST("/api/signout", postSignout)
	e.GET("/api/user/me", getMe)
	e.GET("/api/isu", getIsuList)
	e.POST("/api/isu", postIsu)
	e.GET("/api/isu/:jia_isu_uuid", getIsuID)
	e.GET("/api/isu/:jia_isu_uuid/icon", getIsuIcon)
	e.GET("/api/isu/:jia_isu_uuid/graph", getIsuGraph)
	e.GET("/api/condition/:jia_isu_uuid", getIsuConditions)
	e.GET("/api/trend", getTrend)

	e.POST("/api/condition/:jia_isu_uuid", postIsuCondition)

	e.GET("/", getIndex)
	e.GET("/isu/:jia_isu_uuid", getIndex)
	e.GET("/isu/:jia_isu_uuid/condition", getIndex)
	e.GET("/isu/:jia_isu_uuid/graph", getIndex)
	e.GET("/register", getIndex)
	e.Static("/assets", frontendContentsPath+"/assets")

	mySQLConnectionData = NewMySQLConnectionEnv()

	var err error
	db, err = mySQLConnectionData.ConnectDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	db.SetMaxOpenConns(10)
	defer db.Close()

	postIsuConditionTargetBaseURL = os.Getenv("POST_ISUCONDITION_TARGET_BASE_URL")
	if postIsuConditionTargetBaseURL == "" {
		e.Logger.Fatalf("missing: POST_ISUCONDITION_TARGET_BASE_URL")
		return
	}

	if os.Getenv("ISUCONDITION_WORKER") == "1" {
		isuConditionWorker = newIsuConditionWorker()
		isuConditionWorker.Start(e.Logger)
	}

	serverPort := fmt.Sprintf(":%v", getEnv("SERVER_APP_PORT", "3000"))
	e.Logger.Fatal(e.Start(serverPort))
}

func getSession(r *http.Request) (*sessions.Session, error) {
	session, err := sessionStore.Get(r, sessionName)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func getUserIDFromSession(c echo.Context) (string, int, error) {
	session, err := getSession(c.Request())
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("failed to get session: %v", err)
	}
	_jiaUserID, ok := session.Values["jia_user_id"]
	if !ok {
		return "", http.StatusUnauthorized, fmt.Errorf("no session")
	}

	jiaUserID := _jiaUserID.(string)
	var count int

	err = db.Get(&count, "SELECT COUNT(*) FROM `user` WHERE `jia_user_id` = ?",
		jiaUserID)
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("db error: %v", err)
	}

	if count == 0 {
		return "", http.StatusUnauthorized, fmt.Errorf("not found: user")
	}

	return jiaUserID, 0, nil
}

func getJIAServiceURL() string {
	if jiaServiceURL != "" {
		return jiaServiceURL
	}
	return defaultJIAServiceURL
}

func generateIsuImageDir(jiaUserID string) string {
	return fmt.Sprintf("../icon/%s", jiaUserID)
}

func generateIsuImagePath(jiaUserID string, jiaIsuUUID string) string {
	return fmt.Sprintf("../icon/%s/%s", jiaUserID, jiaIsuUUID)
}

// POST /initialize
// サービスを初期化
func postInitialize(c echo.Context) error {
	var request InitializeRequest
	err := c.Bind(&request)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	cmd := exec.Command("../sql/init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	err = cmd.Run()
	if err != nil {
		c.Logger().Errorf("exec init.sh error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaServiceURL = request.JIAServiceURL

	req, err := http.NewRequest(http.MethodPost, "http://isucondition-2.t.isucon.dev:3000/reset-worker", bytes.NewBufferString(""))
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.Logger().Errorf("failed to request to JIAService: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer res.Body.Close()

	return c.JSON(http.StatusOK, InitializeResponse{
		Language: "go",
	})
}

// POST /reset-worker
// を初期化
func postResetWorker(c echo.Context) error {
	isuConditionWorker.Reset()
	return c.NoContent(http.StatusOK)
}

// POST /api/auth
// サインアップ・サインイン
func postAuthentication(c echo.Context) error {
	reqJwt := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")

	token, err := jwt.Parse(reqJwt, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, jwt.NewValidationError(fmt.Sprintf("unexpected signing method: %v", token.Header["alg"]), jwt.ValidationErrorSignatureInvalid)
		}
		return jiaJWTSigningKey, nil
	})
	if err != nil {
		switch err.(type) {
		case *jwt.ValidationError:
			return c.String(http.StatusForbidden, "forbidden")
		default:
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		c.Logger().Errorf("invalid JWT payload")
		return c.NoContent(http.StatusInternalServerError)
	}
	jiaUserIDVar, ok := claims["jia_user_id"]
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}
	jiaUserID, ok := jiaUserIDVar.(string)
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}

	_, err = db.Exec("INSERT IGNORE INTO user (`jia_user_id`) VALUES (?)", jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session, err := getSession(c.Request())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session.Values["jia_user_id"] = jiaUserID
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// POST /api/signout
// サインアウト
func postSignout(c echo.Context) error {
	_, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session, err := getSession(c.Request())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session.Options = &sessions.Options{MaxAge: -1, Path: "/"}
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// GET /api/user/me
// サインインしている自分自身の情報を取得
func getMe(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	res := GetMeResponse{JIAUserID: jiaUserID}
	return c.JSON(http.StatusOK, res)
}

// GET /api/isu
// ISUの一覧を取得
func getIsuList(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	isuList := []Isu{}
	err = db.Select(
		&isuList,
		"SELECT `isu`.*"+
			" ,`latest_condition`.`jia_isu_uuid` \"latest_condition.jia_isu_uuid\""+
			" ,`latest_condition`.`timestamp` \"latest_condition.timestamp\""+
			" ,`latest_condition`.`is_sitting` \"latest_condition.is_sitting\""+
			" ,`latest_condition`.`condition` \"latest_condition.condition\""+
			" ,`latest_condition`.`message` \"latest_condition.message\""+
			" FROM `isu`"+
			" LEFT JOIN `isu_condition` AS `latest_condition`"+
			" ON `latest_condition`.`jia_isu_uuid` = `isu`.`jia_isu_uuid`"+
			" AND `latest_condition`.`timestamp` = (SELECT MAX(`timestamp`) FROM `isu_condition` WHERE `jia_isu_uuid` = `isu`.`jia_isu_uuid`)"+
			" WHERE `isu`.`jia_user_id` = ?",
		jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	sort.Slice(isuList, func(i, j int) bool {
		return isuList[i].ID > isuList[j].ID
	})

	responseList := []GetIsuListResponse{}
	for _, isu := range isuList {
		var formattedCondition *GetIsuConditionResponse
		if isu.LatestCondition.JIAIsuUUID.Valid {
			conditionLevel, err := calculateConditionLevel(isu.LatestCondition.Condition.String)
			if err != nil {
				c.Logger().Error(err)
				return c.NoContent(http.StatusInternalServerError)
			}

			formattedCondition = &GetIsuConditionResponse{
				JIAIsuUUID:     isu.JIAIsuUUID,
				IsuName:        isu.Name,
				Timestamp:      isu.LatestCondition.Timestamp.Time.Unix(),
				IsSitting:      isu.LatestCondition.IsSitting.Bool,
				Condition:      isu.LatestCondition.Condition.String,
				ConditionLevel: conditionLevel,
				Message:        isu.LatestCondition.Message.String,
			}
		}

		res := GetIsuListResponse{
			ID:                 isu.ID,
			JIAIsuUUID:         isu.JIAIsuUUID,
			Name:               isu.Name,
			Character:          isu.Character,
			LatestIsuCondition: formattedCondition}
		responseList = append(responseList, res)
	}

	return c.JSON(http.StatusOK, responseList)
}

// POST /api/isu
// ISUを登録
func postIsu(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	useDefaultImage := false

	isu := Isu{
		JIAIsuUUID: c.FormValue("jia_isu_uuid"),
		Name:       c.FormValue("isu_name"),
		JIAUserID:  jiaUserID,
	}

	fh, err := c.FormFile("image")
	if err != nil {
		if !errors.Is(err, http.ErrMissingFile) {
			return c.String(http.StatusBadRequest, "bad format: icon")
		}
		useDefaultImage = true
	}

	targetURL := getJIAServiceURL() + "/api/activate"
	body := JIAServiceRequest{postIsuConditionTargetBaseURL, isu.JIAIsuUUID}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(bodyJSON))
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(reqJIA)
	if err != nil {
		c.Logger().Errorf("failed to request to JIAService: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer res.Body.Close()

	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if res.StatusCode != http.StatusAccepted {
		c.Logger().Errorf("JIAService returned error: status code %v, message: %v", res.StatusCode, string(resBody))
		return c.String(res.StatusCode, "JIAService returned error")
	}

	var isuFromJIA IsuFromJIA
	err = json.Unmarshal(resBody, &isuFromJIA)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	isu.Character = isuFromJIA.Character

	isuRes, err := db.Exec("INSERT INTO `isu`"+
		"	(`jia_isu_uuid`, `name`, `character`, `jia_user_id`)"+
		" VALUES (?, ?, ?, ?)",
		isu.JIAIsuUUID, isu.Name, isu.Character, isu.JIAUserID)
	if err != nil {
		mysqlErr, ok := err.(*mysql.MySQLError)

		if ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			return c.String(http.StatusConflict, "duplicated: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	isuID, err := isuRes.LastInsertId()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	isu.ID = int(isuID)

	os.Mkdir(generateIsuImageDir(isu.JIAUserID), os.ModePerm)
	dst, err := os.Create(generateIsuImagePath(isu.JIAUserID, isu.JIAIsuUUID))
	if err != nil {
		c.Logger().Errorf("file error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer dst.Close()

	if useDefaultImage {
		src, err := os.Open(defaultIconFilePath)
		if err != nil {
			c.Logger().Errorf("file error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
		defer src.Close()
		if _, err := io.Copy(dst, src); err != nil {
			c.Logger().Errorf("file error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
	} else {
		src, err := fh.Open()
		if err != nil {
			c.Logger().Errorf("file error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
		defer src.Close()
		if _, err := io.Copy(dst, src); err != nil {
			c.Logger().Errorf("file error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	return c.JSON(http.StatusCreated, isu)
}

// GET /api/isu/:jia_isu_uuid
// ISUの情報を取得
func getIsuID(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	var res Isu
	err = db.Get(&res, "SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
		jiaUserID, jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSON(http.StatusOK, res)
}

// GET /api/isu/:jia_isu_uuid/icon
// ISUのアイコンを取得
func getIsuIcon(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	file := generateIsuImagePath(jiaUserID, jiaIsuUUID)
	f, err := os.Open(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c.String(http.StatusNotFound, "not found: isu")
		}
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		c.Logger().Errorf("file error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	c.Response().Header().Set(echo.HeaderContentType, "text/plain")
	http.ServeContent(c.Response(), c.Request(), jiaIsuUUID, fi.ModTime(), f)
	return nil
}

// GET /api/isu/:jia_isu_uuid/graph
// ISUのコンディショングラフ描画のための情報を取得
func getIsuGraph(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	datetimeStr := c.QueryParam("datetime")
	if datetimeStr == "" {
		return c.String(http.StatusBadRequest, "missing: datetime")
	}
	datetimeInt64, err := strconv.ParseInt(datetimeStr, 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad format: datetime")
	}
	date := time.Unix(datetimeInt64, 0).Truncate(time.Hour)

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var count int
	err = tx.Get(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
		jiaUserID, jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if count == 0 {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	res, err := generateIsuGraphResponse(tx, jiaIsuUUID, date)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSON(http.StatusOK, res)
}

// グラフのデータ点を一日分生成
func generateIsuGraphResponse(tx *sqlx.Tx, jiaIsuUUID string, graphDate time.Time) ([]GraphResponse, error) {
	endTime := graphDate.Add(time.Hour * 24)

	dataPoints := []GraphDataPointWithInfo{}
	conditionsInThisHour := []IsuCondition{}
	timestampsInThisHour := []int64{}
	var startTimeInThisHour time.Time
	var condition IsuCondition

	rows, err := tx.Queryx("SELECT * FROM `isu_condition`"+
		" WHERE `jia_isu_uuid` = ? AND `timestamp` >= ? AND `timestamp` <= ?"+
		" ORDER BY `timestamp` ASC",
		jiaIsuUUID, graphDate.Truncate(time.Hour).Add(-time.Hour), endTime.Truncate(time.Hour).Add(time.Hour))
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}

	for rows.Next() {
		err = rows.StructScan(&condition)
		if err != nil {
			return nil, err
		}

		truncatedConditionTime := condition.Timestamp.Truncate(time.Hour)
		if truncatedConditionTime != startTimeInThisHour {
			if len(conditionsInThisHour) > 0 {
				data, err := calculateGraphDataPoint(conditionsInThisHour)
				if err != nil {
					return nil, err
				}

				dataPoints = append(dataPoints,
					GraphDataPointWithInfo{
						JIAIsuUUID:          jiaIsuUUID,
						StartAt:             startTimeInThisHour,
						Data:                data,
						ConditionTimestamps: timestampsInThisHour})
			}

			startTimeInThisHour = truncatedConditionTime
			conditionsInThisHour = []IsuCondition{}
			timestampsInThisHour = []int64{}
		}
		conditionsInThisHour = append(conditionsInThisHour, condition)
		timestampsInThisHour = append(timestampsInThisHour, condition.Timestamp.Unix())
	}

	if len(conditionsInThisHour) > 0 {
		data, err := calculateGraphDataPoint(conditionsInThisHour)
		if err != nil {
			return nil, err
		}

		dataPoints = append(dataPoints,
			GraphDataPointWithInfo{
				JIAIsuUUID:          jiaIsuUUID,
				StartAt:             startTimeInThisHour,
				Data:                data,
				ConditionTimestamps: timestampsInThisHour})
	}

	startIndex := len(dataPoints)
	endNextIndex := len(dataPoints)
	for i, graph := range dataPoints {
		if startIndex == len(dataPoints) && !graph.StartAt.Before(graphDate) {
			startIndex = i
		}
		if endNextIndex == len(dataPoints) && graph.StartAt.After(endTime) {
			endNextIndex = i
		}
	}

	filteredDataPoints := []GraphDataPointWithInfo{}
	if startIndex < endNextIndex {
		filteredDataPoints = dataPoints[startIndex:endNextIndex]
	}

	responseList := []GraphResponse{}
	index := 0
	thisTime := graphDate

	for thisTime.Before(endTime) {
		var data *GraphDataPoint
		timestamps := []int64{}

		if index < len(filteredDataPoints) {
			dataWithInfo := filteredDataPoints[index]

			if dataWithInfo.StartAt.Equal(thisTime) {
				data = &dataWithInfo.Data
				timestamps = dataWithInfo.ConditionTimestamps
				index++
			}
		}

		resp := GraphResponse{
			StartAt:             thisTime.Unix(),
			EndAt:               thisTime.Add(time.Hour).Unix(),
			Data:                data,
			ConditionTimestamps: timestamps,
		}
		responseList = append(responseList, resp)

		thisTime = thisTime.Add(time.Hour)
	}

	return responseList, nil
}

// 複数のISUのコンディションからグラフの一つのデータ点を計算
func calculateGraphDataPoint(isuConditions []IsuCondition) (GraphDataPoint, error) {
	conditionsCount := map[string]int{"is_broken": 0, "is_dirty": 0, "is_overweight": 0}
	rawScore := 0
	for _, condition := range isuConditions {
		badConditionsCount := 0

		if !isValidConditionFormat(condition.Condition) {
			return GraphDataPoint{}, fmt.Errorf("invalid condition format")
		}

		for _, condStr := range strings.Split(condition.Condition, ",") {
			keyValue := strings.Split(condStr, "=")

			conditionName := keyValue[0]
			if keyValue[1] == "true" {
				conditionsCount[conditionName] += 1
				badConditionsCount++
			}
		}

		if badConditionsCount >= 3 {
			rawScore += scoreConditionLevelCritical
		} else if badConditionsCount >= 1 {
			rawScore += scoreConditionLevelWarning
		} else {
			rawScore += scoreConditionLevelInfo
		}
	}

	sittingCount := 0
	for _, condition := range isuConditions {
		if condition.IsSitting {
			sittingCount++
		}
	}

	isuConditionsLength := len(isuConditions)

	score := rawScore * 100 / 3 / isuConditionsLength

	sittingPercentage := sittingCount * 100 / isuConditionsLength
	isBrokenPercentage := conditionsCount["is_broken"] * 100 / isuConditionsLength
	isOverweightPercentage := conditionsCount["is_overweight"] * 100 / isuConditionsLength
	isDirtyPercentage := conditionsCount["is_dirty"] * 100 / isuConditionsLength

	dataPoint := GraphDataPoint{
		Score: score,
		Percentage: ConditionsPercentage{
			Sitting:      sittingPercentage,
			IsBroken:     isBrokenPercentage,
			IsOverweight: isOverweightPercentage,
			IsDirty:      isDirtyPercentage,
		},
	}
	return dataPoint, nil
}

// GET /api/condition/:jia_isu_uuid
// ISUのコンディションを取得
func getIsuConditions(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	endTimeInt64, err := strconv.ParseInt(c.QueryParam("end_time"), 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad format: end_time")
	}
	endTime := time.Unix(endTimeInt64, 0)
	conditionLevelCSV := c.QueryParam("condition_level")
	if conditionLevelCSV == "" {
		return c.String(http.StatusBadRequest, "missing: condition_level")
	}
	scoreConditionLevels := []int{}
	for _, level := range strings.Split(conditionLevelCSV, ",") {
		switch level {
		case conditionLevelInfo:
			scoreConditionLevels = append(scoreConditionLevels, scoreConditionLevelInfo)
		case conditionLevelWarning:
			scoreConditionLevels = append(scoreConditionLevels, scoreConditionLevelWarning)
		case conditionLevelCritical:
			scoreConditionLevels = append(scoreConditionLevels, scoreConditionLevelCritical)
		}
	}

	startTimeStr := c.QueryParam("start_time")
	var startTime time.Time
	if startTimeStr != "" {
		startTimeInt64, err := strconv.ParseInt(startTimeStr, 10, 64)
		if err != nil {
			return c.String(http.StatusBadRequest, "bad format: start_time")
		}
		startTime = time.Unix(startTimeInt64, 0)
	}

	var isuName string
	err = db.Get(&isuName,
		"SELECT name FROM `isu` WHERE `jia_isu_uuid` = ? AND `jia_user_id` = ?",
		jiaIsuUUID, jiaUserID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	conditionsResponse, err := getIsuConditionsFromDB(db, jiaIsuUUID, endTime, scoreConditionLevels, startTime, conditionLimit, isuName)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.JSON(http.StatusOK, conditionsResponse)
}

// ISUのコンディションをDBから取得
func getIsuConditionsFromDB(db *sqlx.DB, jiaIsuUUID string, endTime time.Time, scoreConditionLevels []int, startTime time.Time,
	limit int, isuName string) ([]*GetIsuConditionResponse, error) {

	conditions := []IsuCondition{}
	var err error

	if startTime.IsZero() {
		args := []interface{}{jiaIsuUUID, endTime}
		for _, scoreConditionLevel := range scoreConditionLevels {
			args = append(args, scoreConditionLevel)
		}
		args = append(args, limit)
		err = db.Select(&conditions,
			"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"	AND `score_condition_level` IN ("+strings.Repeat("?, ", len(scoreConditionLevels)-1)+"?)"+
				"	ORDER BY `timestamp` DESC LIMIT ?",
			args...,
		)
	} else {
		args := []interface{}{jiaIsuUUID, endTime, startTime}
		for _, scoreConditionLevel := range scoreConditionLevels {
			args = append(args, scoreConditionLevel)
		}
		args = append(args, limit)
		err = db.Select(&conditions,
			"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"	AND ? <= `timestamp`"+
				"	AND `score_condition_level` IN ("+strings.Repeat("?, ", len(scoreConditionLevels)-1)+"?)"+
				"	ORDER BY `timestamp` DESC LIMIT ?",
			args...,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}

	conditionsResponse := []*GetIsuConditionResponse{}
	for _, c := range conditions {
		cLevel, err := calculateConditionLevel(c.Condition)
		if err != nil {
			continue
		}

		data := GetIsuConditionResponse{
			JIAIsuUUID:     c.JIAIsuUUID,
			IsuName:        isuName,
			Timestamp:      c.Timestamp.Unix(),
			IsSitting:      c.IsSitting,
			Condition:      c.Condition,
			ConditionLevel: cLevel,
			Message:        c.Message,
		}
		conditionsResponse = append(conditionsResponse, &data)
	}

	return conditionsResponse, nil
}

// ISUのコンディションの文字列からコンディションレベルを計算
func calculateConditionLevel(condition string) (string, error) {
	var conditionLevel string

	scoreConditionLevel, err := calculateScoreConditionLevel(condition)
	if err != nil {
		return "", err
	}

	switch scoreConditionLevel {
	case scoreConditionLevelInfo:
		conditionLevel = conditionLevelInfo
	case scoreConditionLevelWarning:
		conditionLevel = conditionLevelWarning
	case scoreConditionLevelCritical:
		conditionLevel = conditionLevelCritical
	default:
		return "", fmt.Errorf("unexpected warn count")
	}

	return conditionLevel, nil
}

// ISUのコンディションの文字列からコンディションレベルスコアを計算
func calculateScoreConditionLevel(condition string) (int, error) {
	var scoreConditionLevel int

	warnCount := strings.Count(condition, "=true")
	switch warnCount {
	case 0:
		scoreConditionLevel = scoreConditionLevelInfo
	case 1, 2:
		scoreConditionLevel = scoreConditionLevelWarning
	case 3:
		scoreConditionLevel = scoreConditionLevelCritical
	default:
		return 0, fmt.Errorf("unexpected warn count")
	}

	return scoreConditionLevel, nil
}

// GET /api/trend
// ISUの性格毎の最新のコンディション情報
func getTrend(c echo.Context) error {
	return c.JSON(http.StatusOK, isuConditionWorker.GetTrendResponses())
}

// POST /api/condition/:jia_isu_uuid
// ISUからのコンディションを受け取る
func postIsuCondition(c echo.Context) error {
	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	req := []PostIsuConditionRequest{}
	err := c.Bind(&req)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	} else if len(req) == 0 {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	var count int
	err = db.Get(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_isu_uuid` = ?", jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if count == 0 {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	isuConditions := []*IsuCondition{}
	for _, cond := range req {
		timestamp := time.Unix(cond.Timestamp, 0)

		scoreConditionLevel, err := calculateScoreConditionLevel(cond.Condition)
		if !isValidConditionFormat(cond.Condition) || err != nil {
			return c.String(http.StatusBadRequest, "bad request body")
		}

		isuConditions = append(isuConditions, &IsuCondition{
			JIAIsuUUID:          jiaIsuUUID,
			Timestamp:           timestamp,
			IsSitting:           cond.IsSitting,
			Condition:           cond.Condition,
			ScoreConditionLevel: scoreConditionLevel,
			Message:             cond.Message,
		})
	}
	isuConditionWorker.AppendIsuConditions(isuConditions)

	return c.NoContent(http.StatusAccepted)
}

// ISUのコンディションの文字列がcsv形式になっているか検証
func isValidConditionFormat(conditionStr string) bool {

	keys := []string{"is_dirty=", "is_overweight=", "is_broken="}
	const valueTrue = "true"
	const valueFalse = "false"

	idxCondStr := 0

	for idxKeys, key := range keys {
		if !strings.HasPrefix(conditionStr[idxCondStr:], key) {
			return false
		}
		idxCondStr += len(key)

		if strings.HasPrefix(conditionStr[idxCondStr:], valueTrue) {
			idxCondStr += len(valueTrue)
		} else if strings.HasPrefix(conditionStr[idxCondStr:], valueFalse) {
			idxCondStr += len(valueFalse)
		} else {
			return false
		}

		if idxKeys < (len(keys) - 1) {
			if conditionStr[idxCondStr] != ',' {
				return false
			}
			idxCondStr++
		}
	}

	return (idxCondStr == len(conditionStr))
}

func getIndex(c echo.Context) error {
	return c.File(frontendContentsPath + "/index.html")
}
