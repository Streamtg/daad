package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"webBridgeBot/internal/config"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/tg"
)

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
}

const permanentAdminID int64 = 8030036884 // TU ID – ADMIN ÚNICO Y PERMANENTE

func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)

	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:          true,
			Session:           sessionMaker.SqlSession(sqlite.Open(dsn)),
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Telegram client: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	userRepository := data.NewUserRepository(db)
	if err := userRepository.InitDB(); err != nil {
		return nil, err
	}

	tgCtx := tgClient.CreateContext()
	webServer := web.NewServer(config, tgClient, tgCtx, log, userRepository)

	return &TelegramBot{
		config:         config,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepository,
		db:             db,
		webServer:      webServer,
	}, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to start Telegram client: %s", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	// admin dump
	d.AddHandler(handlers.NewCommand("dumpdb", b.handleDumpDB))
}

// ==================== /start ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAuthorized := true
	isAdmin := user.ID == permanentAdminID

	if err := b.userRepository.StoreUserInfo(
		user.ID,
		u.EffectiveChat().GetID(),
		user.FirstName,
		user.LastName,
		user.Username,
		isAuthorized,
		isAdmin,
	); err != nil {
		b.logger.Printf("Failed to store user %d: %v", user.ID, err)
		return err
	}

	// sincronizar (exportar DB y actualizar fragmentos en canal)
	if err := b.backupDBToChannelMulti(); err != nil {
		b.logger.Printf("Failed to backup DB after /start: %v", err)
	}

	welcome := `Send or forward any multimedia file (audio or video) and I will instantly generate a direct streaming link for you at lightning speed.

Supported formats:
• Audio: MP3, M4A, FLAC, WAV, OGG...
• Video: MP4, MKV, AVI, MOV, WEBM...
• Photos & documents (sent as files)

How to use me:
• Personal media host (movies, series, documentaries)
• Share large videos without Telegram compression
• Build your private streaming library
• Stream directly in browser from any device
• Access your files anywhere, anytime

Just send me a file — magic happens instantly! 
Support: @Wavetouch_bot`

	return b.sendReply(ctx, u, welcome)
}

// ==================== /ban ====================
func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /ban <user_id> [reason]")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || targetID <= 0 {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}
	if targetID == permanentAdminID {
		return b.sendReply(ctx, u, "You cannot ban the main administrator.")
	}

	reason := "No reason provided"
	if len(args) > 2 {
		reason = strings.Join(args[2:], " ")
	}

	if err := b.userRepository.DeauthorizeUser(targetID); err != nil {
		return b.sendReply(ctx, u, "Failed to ban user.")
	}

	// sincronizar DB al canal
	if err := b.backupDBToChannelMulti(); err != nil {
		b.logger.Printf("Failed to backup DB after /ban: %v", err)
	}

	b.logger.Printf("ADMIN %d banned user %d – Reason: %s", permanentAdminID, targetID, reason)

	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			_, _ = b.tgClient.API().MessagesSendMessage(
				b.tgCtx,
				&tg.MessagesSendMessageRequest{
					Peer:    peer,
					Message: fmt.Sprintf("You have been permanently banned from using this bot.\nSupport: @Wavetouch_bot\n\nReason: %s", reason),
				},
			)
		}
	}()

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nSupport: @Wavetouch_bot\nReason: %s", targetID, reason))
}

// ==================== /unban ====================
func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /unban <user_id>")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || targetID <= 0 {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	if err := b.userRepository.AuthorizeUser(targetID, false); err != nil {
		return b.sendReply(ctx, u, "Failed to unban user.")
	}

	// sincronizar DB al canal
	if err := b.backupDBToChannelMulti(); err != nil {
		b.logger.Printf("Failed to backup DB after /unban: %v", err)
	}

	b.logger.Printf("ADMIN %d unbanned user %d", permanentAdminID, targetID)

	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			_, _ = b.tgClient.API().MessagesSendMessage(
				b.tgCtx,
				&tg.MessagesSendMessageRequest{
					Peer:    peer,
					Message: "You have been unbanned!\nYou can now use the bot again.",
				},
			)
		}
	}()

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
}

// ==================== /listusers ====================
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}

	const pageSize = 10
	page := 1
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) > 1 {
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
			page = p
		}
	}

	total, _ := b.userRepository.GetUserCount()
	if total == 0 {
		return b.sendReply(ctx, u, "No users registered yet.")
	}

	offset := (page - 1) * pageSize
	users, _ := b.userRepository.GetAllUsers(offset, pageSize)
	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users on this page.")
	}

	var msg strings.Builder
	msg.WriteString("*User List*\n\n")
	for i, usr := range users {
		status := "Authorized"
		if !usr.IsAuthorized {
			status = "Banned"
		}
		adminTag := ""
		if usr.IsAdmin {
			adminTag = " (Admin)"
		}
		username := usr.Username
		if username == "" {
			username = "N/A"
		}
		msg.WriteString(fmt.Sprintf("%d. `%d` – %s %s (@%s) – %s%s\n",
			offset+i+1, usr.UserID, usr.FirstName, usr.LastName, username, status, adminTag))
	}
	totalPages := (total + pageSize - 1) / pageSize
	msg.WriteString(fmt.Sprintf("\nPage %d of %d (%d total users)", page, totalPages, total))

	return b.sendReply(ctx, u, msg.String())
}

// ==================== /userinfo ====================
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	target, err := b.userRepository.GetUserInfo(targetID)
	if err != nil || target == nil {
		return b.sendReply(ctx, u, "User not found.")
	}

	status := "Authorized"
	if !target.IsAuthorized {
		status = "Banned"
	}
	adminStatus := "No"
	if target.IsAdmin {
		adminStatus = "Yes"
	}
	username := target.Username
	if username == "" {
		username = "N/A"
	}

	msg := fmt.Sprintf(`*User Information*
ID: <code>%d</code>
Name: %s %s
Username: @%s
Status: %s
Admin: %s
Joined: %s`,
		target.UserID, target.FirstName, target.LastName, username, status, adminStatus, target.CreatedAt)

	return b.sendReply(ctx, u, msg)
}

// ==================== Media & resto ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if webPage, ok := u.EffectiveMessage.Message.Media.(*tg.MessageMediaWebPage); ok {
			if _, empty := webPage.Webpage.(*tg.WebPageEmpty); empty {
				if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
					mime := utils.DetectMimeTypeFromURL(link)
					file = &types.DocumentFile{
						FileName: "external_link",
						MimeType: mime,
						FileSize: 0,
					}
					return b.sendMediaToUser(ctx, u, link, file, false)
				}
			}
		}
		return b.sendReply(ctx, u, "Unsupported file or link.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error { return nil }

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL}}},
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	if err != nil {
		b.logger.Printf("Failed to send media reply: %v", err)
		return err
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	proxied := b.wrapWithProxyIfNeeded(fileURL)
	return map[string]string{
		"url":         proxied,
		"fileName":    file.FileName,
		"fileId":      strconv.FormatInt(file.ID, 10),
		"mimeType":    file.MimeType,
		"duration":    strconv.Itoa(file.Duration),
		"width":       strconv.Itoa(file.Width),
		"height":      strconv.Itoa(file.Height),
		"title":       file.Title,
		"performer":   file.Performer,
		"isVoice":     strconv.FormatBool(file.IsVoice),
		"isAnimation": strconv.FormatBool(file.IsAnimation),
	}
}

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	if strings.HasPrefix(fileURL, "http://") || strings.HasPrefix(fileURL, "https://") {
		if !strings.Contains(fileURL, ":"+b.config.Port) &&
			!strings.Contains(fileURL, "localhost") &&
			!strings.HasPrefix(fileURL, b.config.BaseURL) {
			return "/proxy?url=" + url.QueryEscape(fileURL)
		}
	}
	return fileURL
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Reply error: %v", err)
	}
	return err
}

//
// --------------------- DB <-> Channel multi-message sync helpers ---------------------
//

// Canal de logs (ya definido por ti): -1003213143951
const logChannelID int64 = -1003213143951

type userDump struct {
	UserID       int64  `json:"user_id"`
	ChatID       int64  `json:"chat_id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	IsAuthorized bool   `json:"is_authorized"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    string `json:"created_at"`
}

// exportDatabaseToJSON lee la tabla users local usando userRepository y devuelve JSON indentado
func (b *TelegramBot) exportDatabaseToJSON() ([]byte, error) {
	users, err := b.userRepository.GetAllUsers(0, 1000000)
	if err != nil {
		return nil, err
	}

	var dump []userDump
	for _, u := range users {
		dump = append(dump, userDump{
			UserID:       u.UserID,
			ChatID:       u.ChatID,
			FirstName:    u.FirstName,
			LastName:     u.LastName,
			Username:     u.Username,
			IsAuthorized: u.IsAuthorized,
			IsAdmin:      u.IsAdmin,
			CreatedAt:    u.CreatedAt,
		})
	}

	return json.MarshalIndent(dump, "", "  ")
}

// importDatabaseFromJSON reemplaza la tabla users a partir del JSON recibido
func (b *TelegramBot) importDatabaseFromJSON(data []byte) error {
	var users []userDump
	if err := json.Unmarshal(data, &users); err != nil {
		return err
	}

	// Limpiar tabla
	if _, err := b.db.Exec("DELETE FROM users"); err != nil {
		return err
	}

	for _, u := range users {
		if err := b.userRepository.StoreUserInfo(
			u.UserID,
			u.ChatID,
			u.FirstName,
			u.LastName,
			u.Username,
			u.IsAuthorized,
			u.IsAdmin,
		); err != nil {
			b.logger.Printf("Failed to store user %d from import: %v", u.UserID, err)
		}
	}

	return nil
}

// splitToChunks divide texto en trozos de tamaño máximo chunkSize (runes)
func splitToChunks(s string, chunkSize int) []string {
	var chunks []string
	runes := []rune(s)
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

// extractMessagesFromHistory intenta obtener slice de mensajes desde el resultado de MessagesGetHistory
func extractMessagesFromHistory(history interface{}) []reflect.Value {
	v := reflect.ValueOf(history)
	if !v.IsValid() {
		return nil
	}
	// Si es puntero, desreferenciar
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	// Buscar campo Messages
	f := v.FieldByName("Messages")
	if f.IsValid() && f.Kind() == reflect.Slice {
		var out []reflect.Value
		for i := 0; i < f.Len(); i++ {
			out = append(out, f.Index(i))
		}
		return out
	}
	// Buscar método GetMessages()
	m := v.MethodByName("GetMessages")
	if m.IsValid() {
		ret := m.Call(nil)
		if len(ret) > 0 {
			r := ret[0]
			if r.Kind() == reflect.Slice {
				var out []reflect.Value
				for i := 0; i < r.Len(); i++ {
					out = append(out, r.Index(i))
				}
				return out
			}
		}
	}
	return nil
}

// helper to read message text (works with both string and *string etc.) and ID (int/ *int)
func readMessageTextAndID(rv reflect.Value) (string, int, bool) {
	if !rv.IsValid() {
		return "", 0, false
	}
	// si es interfaz, obtener elemento
	if rv.Kind() == reflect.Interface || rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return "", 0, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return "", 0, false
	}
	// Message (string)
	text := ""
	fm := rv.FieldByName("Message")
	if fm.IsValid() {
		switch fm.Kind() {
		case reflect.String:
			text = fm.String()
		case reflect.Ptr:
			if !fm.IsNil() {
				if fm.Elem().Kind() == reflect.String {
					text = fm.Elem().String()
				}
			}
		}
	}
	// ID or Id (int)
	id := 0
	fi := rv.FieldByName("ID")
	if fi.IsValid() {
		switch fi.Kind() {
		case reflect.Int, reflect.Int32, reflect.Int64:
			id = int(fi.Int())
		case reflect.Ptr:
			if !fi.IsNil() {
				id = int(fi.Elem().Int())
			}
		}
	} else {
		fi2 := rv.FieldByName("Id")
		if fi2.IsValid() {
			switch fi2.Kind() {
			case reflect.Int, reflect.Int32, reflect.Int64:
				id = int(fi2.Int())
			case reflect.Ptr:
				if !fi2.IsNil() {
					id = int(fi2.Elem().Int())
				}
			}
		}
	}
	// if text empty and id zero -> invalid?
	if text == "" && id == 0 {
		return "", 0, false
	}
	return text, id, true
}

// collectDBFragmentsFromHistory busca en el history del canal todos los mensajes que sean DB PART y devuelve sus ids ordenados por parte
func (b *TelegramBot) collectDBFragmentsFromHistory() ([]int, error) {
	peer := b.tgCtx.PeerStorage.GetInputPeerById(logChannelID)
	if peer == nil {
		return nil, fmt.Errorf("log channel peer not found")
	}

	allFragments := map[int]int{} // part -> message id

	// paginar el history hacia atrás en bloques
	limit := 100
	offsetID := 0
	for {
		history, err := b.tgClient.API().MessagesGetHistory(
			b.tgCtx,
			&tg.MessagesGetHistoryRequest{
				Peer:     peer,
				Limit:    limit,
				OffsetID: offsetID,
			},
		)
		if err != nil {
			return nil, err
		}

		msgVals := extractMessagesFromHistory(history)
		if len(msgVals) == 0 {
			break
		}

		minID := 0
		for _, mv := range msgVals {
			text, id, ok := readMessageTextAndID(mv)
			if !ok {
				continue
			}
			if strings.HasPrefix(text, "📚 DB PART ") {
				lines := strings.SplitN(text, "\n", 2)
				header := lines[0]
				parts := strings.Fields(header)
				if len(parts) >= 4 {
					partStr := parts[3] // "X/Y"
					if strings.Contains(partStr, "/") {
						p := strings.SplitN(partStr, "/", 2)
						if idx, err := strconv.Atoi(p[0]); err == nil {
							allFragments[idx] = id
						}
					}
				}
			}
			if minID == 0 || (id > 0 && id < minID) {
				minID = id
			}
		}

		// si no podemos ir más atrás, rompemos
		if minID <= 1 {
			break
		}
		// preparar siguiente paginación: pedir mensajes anteriores a minID-1
		offsetID = minID - 1
	}

	if len(allFragments) == 0 {
		return nil, nil
	}

	// construir lista ordenada
	keys := make([]int, 0, len(allFragments))
	for k := range allFragments {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var ids []int
	for _, k := range keys {
		ids = append(ids, allFragments[k])
	}
	return ids, nil
}

// deleteMessages borra mensajes propios por id (best-effort)
func (b *TelegramBot) deleteMessages(ids []int) error {
	if len(ids) == 0 {
		return nil
	}

	// intentar con campo ID []int
	_, err := b.tgClient.API().MessagesDeleteMessages(
		b.tgCtx,
		&tg.MessagesDeleteMessagesRequest{
			ID:     ids,
			Revoke: true,
		},
	)
	if err == nil {
		return nil
	}

	// fallback: convertir a []int32 si algún build lo requiere
	int32ids := make([]int32, 0, len(ids))
	for _, v := range ids {
		int32ids = append(int32ids, int32(v))
	}
	_, err2 := b.tgClient.API().MessagesDeleteMessages(
		b.tgCtx,
		&tg.MessagesDeleteMessagesRequest{
			// try field name Id if present in some builds (reflection not possible here)
			// Many builds accept Id []int32: we pass Id via a type that will match the API at runtime
			Revoke: true,
			// this literal will not compile if tg.MessagesDeleteMessagesRequest doesn't have Id field;
			// we rely on primary attempt above; this second attempt may fail at runtime but it's a best-effort.
			// To keep compilation safe, we don't set Id here.
		},
	)
	if err2 == nil {
		return nil
	}

	b.logger.Printf("Failed to delete old DB fragment messages: %v / %v", err, err2)
	if err != nil {
		return err
	}
	return err2
}

// helper para enviar mensaje seguro (usa sendMessage con ctx)
func (b *TelegramBot) sendMessageToPeer(peer tg.InputPeerClass, text string) error {
	_, err := b.tgClient.API().MessagesSendMessage(
		b.tgCtx,
		&tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: text,
		},
	)
	return err
}

// backupDBToChannelMulti exporta DB y la guarda fragmentada en mensajes del canal de logs
func (b *TelegramBot) backupDBToChannelMulti() error {
	data, err := b.exportDatabaseToJSON()
	if err != nil {
		return err
	}
	jsonText := string(data)

	// fragment size: dejar margen para encabezado "📚 DB PART X/Y\n"
	const chunkSize = 3800
	chunks := splitToChunks(jsonText, chunkSize)
	total := len(chunks)
	if total == 0 {
		return nil
	}

	peer := b.tgCtx.PeerStorage.GetInputPeerById(logChannelID)
	if peer == nil {
		return fmt.Errorf("log channel peer not found")
	}

	// 1) recoger mensajes antiguos de DB (si existen) y borrarlos (limpiar y reescribir)
	oldIDs, err := b.collectDBFragmentsFromHistory()
	if err == nil && len(oldIDs) > 0 {
		_ = b.deleteMessages(oldIDs)
		// pequeña espera para evitar race
		time.Sleep(200 * time.Millisecond)
	}

	// 2) enviar cada fragmento como mensaje propio
	var newIDs []int
	for i, ch := range chunks {
		header := fmt.Sprintf("📚 DB PART %d/%d\n", i+1, total)
		text := header + ch
		_, err := b.tgClient.API().MessagesSendMessage(
			b.tgCtx,
			&tg.MessagesSendMessageRequest{
				Peer:    peer,
				Message: text,
			},
		)
		if err != nil {
			b.logger.Printf("Failed to send DB fragment %d: %v", i+1, err)
			continue
		}
		// breve pausa para indexación
		time.Sleep(120 * time.Millisecond)
		// mejor esfuerzo: recolectar ids actuales
		idsNow, _ := b.collectDBFragmentsFromHistory()
		if len(idsNow) > 0 {
			newIDs = idsNow
		}
	}

	// actualizar cache local (opcional)
	if len(newIDs) > 0 {
		b.logger.Printf("DB fragments updated: %d parts stored", len(newIDs))
	}

	return nil
}

// restoreDBFromChannelMulti busca todas las partes, las concatena y restaura la DB local
func (b *TelegramBot) restoreDBFromChannelMulti() error {
	peer := b.tgCtx.PeerStorage.GetInputPeerById(logChannelID)
	if peer == nil {
		return fmt.Errorf("log channel peer not found")
	}

	ids, err := b.collectDBFragmentsFromHistory()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		// no fragments, crear uno inicial vacío (1/1)
		initial := "📚 DB PART 1/1\n[]"
		_, err := b.tgClient.API().MessagesSendMessage(
			b.tgCtx,
			&tg.MessagesSendMessageRequest{
				Peer:    peer,
				Message: initial,
			},
		)
		if err != nil {
			return fmt.Errorf("failed to create initial DB message: %w", err)
		}
		// intentar recollect
		ids, _ = b.collectDBFragmentsFromHistory()
		if len(ids) == 0 {
			return nil
		}
	}

	// obtener history amplio y extraer fragments con su texto
	history, err := b.tgClient.API().MessagesGetHistory(
		b.tgCtx,
		&tg.MessagesGetHistoryRequest{
			Peer:  peer,
			Limit: 500,
		},
	)
	if err != nil {
		return err
	}

	msgVals := extractMessagesFromHistory(history)
	type frag struct {
		Index int
		Total int
		Text  string
		ID    int
	}
	var fragments []frag

	for _, mv := range msgVals {
		text, id, ok := readMessageTextAndID(mv)
		if !ok {
			continue
		}
		if !strings.HasPrefix(text, "📚 DB PART ") {
			continue
		}
		lines := strings.SplitN(text, "\n", 2)
		header := lines[0]
		body := ""
		if len(lines) > 1 {
			body = lines[1]
		}
		parts := strings.Fields(header)
		if len(parts) >= 4 {
			partStr := parts[3]
			if strings.Contains(partStr, "/") {
				p := strings.SplitN(partStr, "/", 2)
				if idx, err := strconv.Atoi(p[0]); err == nil {
					total := 0
					if t, err2 := strconv.Atoi(p[1]); err2 == nil {
						total = t
					}
					fragments = append(fragments, frag{
						Index: idx,
						Total: total,
						Text:  body,
						ID:    id,
					})
				}
			}
		}
	}

	if len(fragments) == 0 {
		return fmt.Errorf("no DB fragments found in channel history")
	}

	// ordenar por Index y concatenar
	sort.Slice(fragments, func(i, j int) bool { return fragments[i].Index < fragments[j].Index })
	var builder strings.Builder
	for _, f := range fragments {
		builder.WriteString(f.Text)
	}
	combined := builder.String()

	// importar a BD local
	if err := b.importDatabaseFromJSON([]byte(combined)); err != nil {
		return fmt.Errorf("failed to import DB from combined fragments: %w", err)
	}

	b.logger.Printf("Database restored from %d fragments (total bytes: %d)", len(fragments), len(combined))
	return nil
}

// ==================== /dumpdb (admin) ====================
func (b *TelegramBot) handleDumpDB(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}

	peer := b.tgCtx.PeerStorage.GetInputPeerById(u.EffectiveChat().GetID())
	if peer == nil {
		return b.sendReply(ctx, u, "Could not get peer for admin chat.")
	}

	ids, err := b.collectDBFragmentsFromHistory()
	if err != nil {
		return b.sendReply(ctx, u, "Failed to fetch DB fragments.")
	}
	if len(ids) == 0 {
		return b.sendReply(ctx, u, "No DB fragments found.")
	}

	// obtener history amplio y reconstruir
	history, err := b.tgClient.API().MessagesGetHistory(
		b.tgCtx,
		&tg.MessagesGetHistoryRequest{
			Peer:  b.tgCtx.PeerStorage.GetInputPeerById(logChannelID),
			Limit: 500,
		},
	)
	if err != nil {
		return b.sendReply(ctx, u, "Failed to fetch channel history.")
	}

	msgVals := extractMessagesFromHistory(history)
	fragmentsMap := map[int]string{}
	totalParts := 0
	for _, mv := range msgVals {
		text, _, ok := readMessageTextAndID(mv)
		if !ok {
			continue
		}
		if strings.HasPrefix(text, "📚 DB PART ") {
			lines := strings.SplitN(text, "\n", 2)
			header := lines[0]
			body := ""
			if len(lines) > 1 {
				body = lines[1]
			}
			parts := strings.Fields(header)
			if len(parts) >= 4 {
				partStr := parts[3]
				if strings.Contains(partStr, "/") {
					p := strings.SplitN(partStr, "/", 2)
					if idx, err := strconv.Atoi(p[0]); err == nil {
						if t, err2 := strconv.Atoi(p[1]); err2 == nil {
							totalParts = t
						}
						fragmentsMap[idx] = body
					}
				}
			}
		}
	}

	// reconstruir ordenado
	var keys []int
	for k := range fragmentsMap {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var builder strings.Builder
	for _, k := range keys {
		builder.WriteString(fragmentsMap[k])
	}
	combined := builder.String()

	adminChatID := u.EffectiveChat().GetID()
	if len(combined) < 4000 {
		_, _ = b.tgClient.API().MessagesSendMessage(
			b.tgCtx,
			&tg.MessagesSendMessageRequest{
				Peer:    b.tgCtx.PeerStorage.GetInputPeerById(adminChatID),
				Message: "📂 DATABASE DUMP\n" + combined,
			},
		)
	} else {
		_, _ = b.tgClient.API().MessagesSendMessage(
			b.tgCtx,
			&tg.MessagesSendMessageRequest{
				Peer:    b.tgCtx.PeerStorage.GetInputPeerById(adminChatID),
				Message: fmt.Sprintf("Database dump is large (%d bytes). Please check the log channel %d for fragments (total parts: %d).", len(combined), logChannelID, totalParts),
			},
		)
	}

	return nil
}
