package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"

	"webBridgeBot/internal/config"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)

const (
	permanentAdminID int64 = 8030036884
	logChannelID     int64 = -1003213143951 // TU CANAL REAL
	logChannelMarker      = "DB_USER:"
)

type UserInfo struct {
	UserID       int64  `json:"id"`
	ChatID       int64  `json:"chat_id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	IsAuthorized bool   `json:"auth"`
	IsAdmin      bool   `json:"admin"`
	CreatedAt    string `json:"created_at"`
}

type TelegramBot struct {
	config    *config.Configuration
	tgClient  *gotgproto.Client
	tgCtx     *ext.Context
	api       *tg.Client // Cliente API real
	logger    *logger.Logger
	webServer *web.Server

	userCache map[int64]*UserInfo
}

func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to init client: %w", err)
	}

	tgCtx := tgClient.CreateContext()
	api := tgClient.API() // Este es el cliente real que sí tiene todos los métodos

	bot := &TelegramBot{
		config:    config,
		tgClient:  tgClient,
		tgCtx:     tgCtx,
		api:       api,
		logger:    log,
		webServer: web.NewServer(config, tgClient, tgCtx, log, nil),
		userCache: make(map[int64]*UserInfo),
	}

	// Cargar usuarios del canal
	go bot.loadUsersFromLogChannel()

	return bot, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Bot iniciado (@%s) | DB en canal %d", b.tgClient.Self.Username, logChannelID)
	b.registerHandlers()
	go b.webServer.Start()
	_ = b.tgClient.Idle()
}

// ==================== CARGA USUARIOS DESDE CANAL ====================
func (b *TelegramBot) loadUsersFromLogChannel() {
	time.Sleep(10 * time.Second)
	b.logger.Println("Cargando usuarios desde el canal de logs...")

	ctx := context.Background()
	channel := tg.InputChannelClass(&tg.InputChannel{
		ChannelID:  logChannelID,
		AccessHash: 0,
	})

	var offsetID int
	limit := 100

	for {
		history, err := b.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: channel,
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: offsetID}},
		})
		if err != nil {
			b.logger.Printf("Error leyendo canal: %v", err)
			break
		}

		messages, ok := history.(*tg.MessagesChannelMessages)
		if !ok || len(messages.Messages) == 0 {
			break
		}

		for _, msg := range messages.Messages {
			m, ok := msg.(*tg.Message)
			if !ok || m.Message == "" || !strings.HasPrefix(m.Message, logChannelMarker) {
				continue
			}

			jsonStr := strings.TrimPrefix(m.Message, logChannelMarker)
			var user UserInfo
			if json.Unmarshal([]byte(jsonStr), &user) == nil {
				b.userCache[user.UserID] = &user
				if m.ID > offsetID {
					offsetID = m.ID
				}
			}
		}

		if len(messages.Messages) < limit {
			break
		}
	}

	b.logger.Printf("Base de datos cargada: %d usuarios", len(b.userCache))
}

// ==================== GUARDAR EN CANAL ====================
func (b *TelegramBot) saveUserToLogChannel(user *UserInfo) error {
	data, _ := json.Marshal(user)
	msg := logChannelMarker + string(data)

	ctx := context.Background()
	_, err := b.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     &tg.InputPeerChannel{ChannelID: logChannelID},
		Message:  msg,
		RandomID: rand.Int63(),
	})
	return err
}

// ==================== HELPERS ====================
func (b *TelegramBot) getUser(id int64) *UserInfo {
	if u, ok := b.userCache[id]; ok {
		return u
	}
	return nil
}

func (b *TelegramBot) getAllUsers() []*UserInfo {
	list := make([]*UserInfo, 0, len(b.userCache))
	for _, u := range b.userCache {
		list = append(list, u)
	}
	return list
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStart))
	d.AddHandler(handlers.NewCommand("sms", b.handleSMS))
	d.AddHandler(handlers.NewCommand("ban", b.handleBan))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnban))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMedia))
}

// ==================== /sms ====================
func (b *TelegramBot) handleSMS(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.reply(ctx, u, "Solo el admin principal")
	}

	text := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if text == "" {
		return b.reply(ctx, u, "Uso: /sms <mensaje>")
	}

	success := 0
	for _, user := range b.getAllUsers() {
		if !user.IsAuthorized {
			continue
		}
		_, err := b.api.MessagesSendMessage(context.Background(), &tg.MessagesSendMessageRequest{
			Peer:     &tg.InputPeerUser{UserID: user.ChatID},
			Message:  "Mensaje del admin:\n\n" + text,
			RandomID: rand.Int63(),
		})
		if err == nil {
			success++
		}
		time.Sleep(33 * time.Millisecond)
	}

	return b.reply(ctx, u, fmt.Sprintf("Enviado a %d usuarios", success))
}

// ==================== /start ====================
func (b *TelegramBot) handleStart(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	if b.getUser(user.ID) == nil {
		newUser := &UserInfo{
			UserID:       user.ID,
			ChatID:       u.EffectiveChat().GetID(),
			FirstName:    user.FirstName,
			LastName:     user.LastName,
			Username:     user.Username,
			IsAuthorized: user.ID == permanentAdminID,
			IsAdmin:      user.ID == permanentAdminID,
			CreatedAt:    time.Now().Format(time.RFC3339),
		}
		b.userCache[user.ID] = newUser
		_ = b.saveUserToLogChannel(newUser)
	}

	return b.reply(ctx, u, `Envía cualquier video o audio

MKV · MP4 · AVI · WEBM · MP3 · FLAC

Streaming instantáneo en iPhone, Android, PC, TV

Solo envía un archivo`)
}

// ==================== Media ====================
func (b *TelegramBot) handleMedia(ctx *ext.Context, u *ext.Update) error {
	user := b.getUser(u.EffectiveUser().ID)
	if user == nil {
		return b.reply(ctx, u, "Usa /start primero")
	}
	if !user.IsAuthorized && u.EffectiveUser().ID != permanentAdminID {
		return b.reply(ctx, u, "No estás autorizado")
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
			file = &types.DocumentFile{FileName: "video.mkv", MimeType: "video/mp4"}
			return b.sendLink(ctx, u, link, file)
		}
		return b.reply(ctx, u, "Archivo no soportado")
	}

	url := b.generateURL(u.EffectiveMessage.Message.ID, file)
	return b.sendLink(ctx, u, url, file)
}

func (b *TelegramBot) generateURL(msgID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(fmt.Sprintf("%d%s", msgID, file.FileName), 8)
	return fmt.Sprintf("%s/%d/%s", strings.TrimRight(b.config.BaseURL, "/"), msgID, hash)
}

func (b *TelegramBot) sendLink(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{
			Rows: []tg.KeyboardButtonRow{
				{Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonURL{Text: "STREAM NOW", URL: fileURL},
				}},
			},
		},
	})

	wsMsg := map[string]string{
		"url":      b.proxyURL(fileURL),
		"fileName": file.FileName,
		"mimeType": "video/mp4",
	}
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return err
}

func (b *TelegramBot) proxyURL(urlStr string) string {
	if strings.HasPrefix(urlStr, "http") && !strings.Contains(urlStr, b.config.BaseURL) {
		return "/proxy?url=" + url.QueryEscape(urlStr)
	}
	return urlStr
}

// ==================== Admin Commands ====================
func (b *TelegramBot) handleBan(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID { return nil }
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 { return b.reply(ctx, u, "/ban <id>") }
	id, _ := strconv.ParseInt(args[1], 10, 64)
	if user := b.getUser(id); user != nil {
		user.IsAuthorized = false
		_ = b.saveUserToLogChannel(user)
	}
	return b.reply(ctx, u, "Baneado")
}

func (b *TelegramBot) handleUnban(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID { return nil }
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 { return b.reply(ctx, u, "/unban <id>") }
	id, _ := strconv.ParseInt(args[1], 10, 64)
	if user := b.getUser(id); user != nil {
		user.IsAuthorized = true
		_ = b.saveUserToLogChannel(user)
	}
	return b.reply(ctx, u, "Desbaneado")
}

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID { return nil }
	users := b.getAllUsers()
	msg := "*Usuarios:*\n\n"
	for i, user := range users {
		status := "Baneado"
		if user.IsAuthorized { status = "Autorizado" }
		msg += fmt.Sprintf("%d. `%d` – %s – %s\n", i+1, user.UserID, user.FirstName, status)
	}
	return b.reply(ctx, u, msg)
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID { return nil }
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 { return b.reply(ctx, u, "/userinfo <id>") }
	id, _ := strconv.ParseInt(args[1], 10, 64)
	user := b.getUser(id)
	if user == nil { return b.reply(ctx, u, "No encontrado") }
	return b.reply(ctx, u, fmt.Sprintf("ID: %d\nNombre: %s\nAuth: %t", user.UserID, user.FirstName, user.IsAuthorized))
}

func (b *TelegramBot) reply(ctx *ext.Context, u *ext.Update, text string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{})
	return err
}
