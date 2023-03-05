package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/alexandrevicenzi/go-sse"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

type Configuration struct {
	BotToken     string `json:"bot_token"`
	BotDebug     bool   `json:"bot_debug"`
	ServerPath   string `json:"server_path"`
	BindAddress  string `json:"bind_address"`
	GroupLimit   int    `json:"group_limit"`
	FetchUserPic bool   `json:"fetch_user_pic"`
	ServeMap     bool   `json:"enable_map"`
}

type UserLocation struct {
	Id        int64   `json:"id"`
	Name      string  `json:"name"`
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lng"`
	Date      int     `json:"date"`
}

type Message struct {
	Type         string       `json:"type"`
	UserLocation UserLocation `json:"user_location,omitempty"`
	PicPath      string       `json:"picture,omitempty"`
}

type UserInfo struct {
	Id         int64
	UUID       string
	ProfilePic []byte
}

func initDatabase() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "locbot.db")
	if err != nil {
		log.Fatal(err)
	}

	sqlStmt := `
	create table if not exists users (id integer not null primary key, uuid text, picture blob);
	create table if not exists groups (id integer not null primary key, secret text);
	`
	db.Exec(sqlStmt)
	_, err = db.Exec(sqlStmt)
	if err != nil {
		log.Printf("%q: %s\n", err, sqlStmt)
		return db, err
	}
	return db, nil
}

func addUserPic(id int64, pic []byte) (*UserInfo, error) {
	ui := &UserInfo{
		Id:         id,
		UUID:       uuid.NewString(),
		ProfilePic: pic,
	}
	_, err := db.Exec("insert or replace into users(id, uuid, picture) values(?, ?, ?)", id, ui.UUID, pic)
	if err != nil {
		log.Println(err)
	}
	return ui, err
}

func getUserInfoByID(id int64) (*UserInfo, error) {
	row := db.QueryRow("select id, uuid, picture from users where id = ?", id)
	ui := UserInfo{}
	err := row.Scan(&ui.Id, &ui.UUID, &ui.ProfilePic)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	return &ui, err
}

func getUserInfoByUUID(id string) (*UserInfo, error) {
	row := db.QueryRow("select id, uuid, picture from users where uuid = ?", id)
	ui := UserInfo{}
	err := row.Scan(&ui.Id, &ui.UUID, &ui.ProfilePic)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	return &ui, err
}

func deleteUser(id int64) {
	db.Exec("delete from users where id = ?", id)
}

func picHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		return
	}
	path := strings.Split(r.URL.Path, "/")
	if len(path) == 3 && path[1] == "pic" {
		uuid := path[2]
		ui, err := getUserInfoByUUID(uuid)
		if err == nil {
			w.Write(ui.ProfilePic)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func addGroup(id int64, secret string) error {
	_, err := db.Exec("insert or replace into groups(id, secret) values(?, ?)", id, secret)
	if err != nil {
		log.Println(err)
	}
	return err
}

func getSecret(id int64) (string, error) {
	row := db.QueryRow("select secret from groups where id = ?", id)
	var secret string
	err := row.Scan(&secret)
	if err != nil {
		log.Println(err)
	}
	return secret, err
}

func deleteGroup(id int64) error {
	_, err := db.Exec("delete from groups where id = ?", id)
	if err != nil {
		log.Println(err)
	}
	return err
}

var bot *tgbotapi.BotAPI
var db *sql.DB

var configuration Configuration

func getProfilePic(id int64) ([]byte, error) {
	photos, err := bot.GetUserProfilePhotos(tgbotapi.UserProfilePhotosConfig{
		UserID: id,
		Limit:  1,
	})
	if err != nil {
		return nil, err
	}
	if photos.TotalCount == 0 {
		return nil, fmt.Errorf("no photo")
	}
	photo := photos.Photos[0][0]
	url, err := bot.GetFileDirectURL(photo.FileID)
	if err != nil {
		return nil, err
	}

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	return io.ReadAll(resp.Body)
}

func updateLoop(hook string, s *sse.Server) {
	updates := bot.ListenForWebhook("/hook/" + hook)
	for update := range updates {
		// command
		if update.Message != nil && update.Message.IsCommand() {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "")

			// Extract the command from the Message.
			switch update.Message.Command() {
			case "help":
				msg.Text = "I understand /stream [secret] and /stop"
			case "stream":
				secret := strings.TrimSpace(update.Message.CommandArguments())
				secret = strings.ReplaceAll(secret, ".", "")
				secret = strings.ReplaceAll(secret, "/", "")
				if secret != "" {
					secret = strings.Split(secret, " ")[0]
				} else {
					secret = uuid.NewString()
				}
				addGroup(update.Message.Chat.ID, secret)
				msg.Text = "Start sharing to `" + configuration.ServerPath + "/sub/" + secret + "`"
				msg.ParseMode = "markdown"
			case "stop":
				deleteGroup(update.Message.Chat.ID)
				msg.Text = "Stop sharing locations"
			default:
				continue
			}

			bot.Send(msg)
		}

		// quit/block
		if update.MyChatMember != nil {
			newStatus := update.MyChatMember.NewChatMember.Status
			if newStatus == "left" || newStatus == "kicked" {
				deleteGroup(update.MyChatMember.Chat.ID)
				if update.MyChatMember.Chat.IsPrivate() {
					deleteUser(update.MyChatMember.Chat.ID)
				}
			}
		}

		// location edit
		message := update.Message
		if message == nil {
			message = update.EditedMessage
		}
		if message != nil && message.Location != nil {
			chatId := message.Chat.ID
			if channel, err := getSecret(chatId); err == nil {
				user := message.From
				location := message.Location
				msg := Message{
					Type: "user_location",
					UserLocation: UserLocation{
						Id:        user.ID,
						Name:      user.UserName,
						Latitude:  location.Latitude,
						Longitude: location.Longitude,
						Date:      message.Date,
					},
				}
				if configuration.FetchUserPic {
					userInfo, err := getUserInfoByID(user.ID)
					if err != nil {
						result, err := getProfilePic(user.ID)
						if err == nil {
							userInfo, err = addUserPic(user.ID, result)
						}
					}
					if userInfo != nil {
						msg.PicPath = configuration.ServerPath + "/pic/" + userInfo.UUID
					}
				}
				if buf, err := json.Marshal(msg); err == nil {
					s.SendMessage("/sub/"+channel, sse.SimpleMessage(string(buf)))
				}
			}
		}
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	var err error
	// Open config file
	file, err := os.Open("config.json")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	configuration = Configuration{}
	err = decoder.Decode(&configuration)
	if err != nil {
		log.Fatal(err)
	}

	// create telegram bot object
	bot, err = tgbotapi.NewBotAPI(configuration.BotToken)
	if err != nil {
		log.Fatal(err)
	}
	bot.Debug = configuration.BotDebug
	log.Printf("Authorized on account %s", bot.Self.UserName)

	// register WebHook
	hook := uuid.NewString()
	wh, _ := tgbotapi.NewWebhook(configuration.ServerPath + "/hook/" + hook)
	// this doesn't work as expected, fortunately, Telegram remembers the last allowed updates
	// $ curl -d url="some_url" -d allowed_updates='["message","edited_message","my_chat_member"]' https://api.telegram.org/...
	// wh.AllowedUpdates = append(wh.AllowedUpdates, "messages", "edited_messages", "my_chat_member")
	_, err = bot.Request(wh)
	if err != nil {
		log.Fatal(err)
	}

	// optional (here from copy paste)
	info, err := bot.GetWebhookInfo()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("WebHookInfo: %+v\n", info)

	// set commands
	bot.Request(
		tgbotapi.NewSetMyCommands(tgbotapi.BotCommand{
			Command:     "/stream",
			Description: "Start streaming location shared in the group",
		}, tgbotapi.BotCommand{
			Command:     "/stop",
			Description: "Stop current location sharing from the group",
		}, tgbotapi.BotCommand{
			Command:     "/help",
			Description: "Give some help, maybe",
		}))

	// drop the webhook on exit
	defer bot.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: true})

	if info.LastErrorDate != 0 {
		log.Printf("Telegram callback failed: %s", info.LastErrorMessage)
	}

	// Create SSE server
	s := sse.NewServer(&sse.Options{
		Headers: map[string]string{
			// handled by the reverse proxy
			// "Access-Control-Allow-Origin":  "*",
			// "Access-Control-Allow-Methods": "GET, OPTIONS",
			// "Access-Control-Allow-Headers": "Keep-Alive,X-Requested-With,Cache-Control,Content-Type,Last-Event-ID",
		},
	})
	defer s.Shutdown()
	http.Handle("/sub/", s)

	// db
	db, _ = initDatabase()
	defer db.Close()

	// serve user profile pic
	if configuration.FetchUserPic {
		http.HandleFunc("/pic/", picHandler)
	}

	// serve user profile pic
	if configuration.ServeMap {
		fs := http.FileServer(http.Dir("./static"))
		http.Handle("/", fs)
	}

	// bot loop
	go updateLoop(hook, s)

	httpServer := http.Server{
		Addr: configuration.BindAddress,
	}

	go httpServer.ListenAndServe()

	<-ctx.Done()
}
