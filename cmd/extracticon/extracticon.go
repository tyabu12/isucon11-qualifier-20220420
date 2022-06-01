package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

type MySQLConnectionEnv struct {
	Host     string
	Port     string
	User     string
	DBName   string
	Password string
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

func main() {
	mySQLConnectionData := NewMySQLConnectionEnv()

	var err error
	db, err := mySQLConnectionData.ConnectDB()
	if err != nil {
		log.Fatalf("failed to connect db: %v", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT `jia_user_id`, `jia_isu_uuid`, `image` FROM `isu`")
	if err != nil {
		log.Fatalf("db error: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var jiaUserID string
		var jiaIsuUUID string
		var image []byte
		if err := rows.Scan(&jiaUserID, &jiaIsuUUID, &image); err != nil {
			log.Fatalf("db error: %v", err)
		}
		os.Mkdir(fmt.Sprintf("../icon/%s", jiaUserID), os.ModePerm)
		if err := ioutil.WriteFile(fmt.Sprintf("../icon/%s/%s", jiaUserID, jiaIsuUUID), image, os.ModePerm); err != nil {
			log.Fatalf("db error: %v", err)
		}
	}
}
