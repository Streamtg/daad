package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
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

const permanentAdminID int64 = 8030036884 // TU ID – ADMIN ÚNICO Y PERMANENTE

// Channel username provided by user
const logChannelUsername = "@z95470"

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
	// cached peer for log channel (filled lazily)
	logChannelPeer tg.InputPeerClass
	logPeerInit    bool
}

func NewTelegramBot(cfg *config.Configuration, logg *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", cfg.DatabasePath)

	// Seed RNG for RandomID
	rand.Seed(time.Now().UnixNano())

	tgClient, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
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
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	userRepository := data.NewUserRepository(db)
	if err := userRepository.InitDB(); err != nil {
		return nil, err
	}

	tgCtx := tgClient.CreateContext()
	webServer := web.NewServer(cfg, tgClient, tgCtx, logg, userRepository)

	return &TelegramBot{
		config:         cfg,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         logg,
		userRepository: userRepository,
		db:             db,
		webServer:      webServer,
		logPeerInit:    false,
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
	d.AddHandler(handlers.NewCommand("dumpdb", b.handleDumpDB))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// ================= START =================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	chatID := u.EffectiveChat().GetID()
	isAuthorized := true
	isAdmin := user.ID == permanentAdminID

	if err := b.userRepository.StoreUserInfo(
		user.ID,
		chatID,
		user.FirstName,
		user.LastName,
		user.Username,
		isAuthorized,
		isAdmin,
	); err != nil {
		b.logger.Printf("Failed to store user %d: %v", user.ID, err)
		return err
	}

	// attempt sync after registering new user
	go func() {
		if err := b.backupDBToChannelMulti(); err != nil {
			b.logger.Printf("Failed to backup DB after /start: %v", err)
		}
	}()

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

// ================= BAN / UNBAN =================
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

	b.logger.Printf("ADMIN %d banned user %d – Reason: %s", permanentAdminID, targetID, reason)
	_ = b.trySendLog(fmt.Sprintf("ADMIN %d banned user %d – Reason: %s", permanentAdminID, targetID, reason))

	// notify user (best-effort)
	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			_, _ = b.tgClient.API().MessagesSendMessage(
				b.tgCtx,
				&tg.MessagesSendMessageRequest{
					Peer:     peer,
					Message:  fmt.Sprintf("You have been permanently banned from using this bot.\nReason: %s", reason),
					RandomID: rand.Int63(),
				},
			)
		}
	}()

	// sync DB
	go func() {
		if err := b.backupDBToChannelMulti(); err != nil {
			b.logger.Printf("Failed to backup DB after ban: %v", err)
		}
	}()

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nReason: %s", targetID, reason))
}

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

	b.logger.Printf("ADMIN %d unbanned user %d", permanentAdminID, targetID)
	_ = b.trySendLog(fmt.Sprintf("ADMIN %d unbanned user %d", permanentAdminID, targetID))

	// notify user (best-effort)
	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			_, _ = b.tgClient.API().MessagesSendMessage(
				b.tgCtx,
				&tg.MessagesSendMessageRequest{
					Peer:     peer,
					Message:  "You have been unbanned! You can now use the bot again.",
					RandomID: rand.Int63(),
				},
			)
		}
	}()

	// sync DB
	go func() {
		if err := b.backupDBToChannelMulti(); err != nil {
			b.logger.Printf("Failed to backup DB after unban: %v", err)
		}
	}()

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
}

// ================= LIST USERS / USERINFO =================
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

// ================= MEDIA HANDLING & LOG FORWARD =================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	// Forward message to log channel and send user info as reply in channel (background)
	if err := b.forwardToLogChannelAsync(ctx, u); err != nil {
		b.logger.Printf("Failed background forward to log channel: %v", err)
	}

	file, ferr := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if ferr != nil {
		// try external link from MessageMediaWebPage
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

// ================= HELPERS: WS message, URL generation, replies =================
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

// ================= DB <-> Channel sync helpers =================

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

func (b *TelegramBot) importDatabaseFromJSON(data []byte) error {
	var users []userDump
	if err := json.Unmarshal(data, &users); err != nil {
		return err
	}
	// clear table
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

// splits a string into rune-chunks of size chunkSize
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

// collect DB fragment message IDs from channel history (best-effort)
func (b *TelegramBot) collectDBFragmentsFromHistory() ([]int, error) {
	peer, err := b.ensureLogPeer()
	if err != nil {
		return nil, err
	}
	allFragments := map[int]int{} // part -> message id

	limit := 100
	offsetID := 0
	for {
		history, err := b.tgClient.API().MessagesGetHistory(b.tgCtx, &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			Limit:    limit,
			OffsetID: offsetID,
		})
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
					partStr := parts[3]
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
		if minID <= 1 {
			break
		}
		offsetID = minID - 1
	}
	if len(allFragments) == 0 {
		return nil, nil
	}
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

// delete messages best-effort
func (b *TelegramBot) deleteMessages(ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := b.tgClient.API().MessagesDeleteMessages(b.tgCtx, &tg.MessagesDeleteMessagesRequest{
		ID:     ids,
		Revoke: true,
	})
	if err == nil {
		return nil
	}
	// fallback: try without ID typed (best-effort)
	b.logger.Printf("deleteMessages primary failed: %v", err)
	return err
}

// export DB to channel (fragmented)
func (b *TelegramBot) backupDBToChannelMulti() error {
	data, err := b.exportDatabaseToJSON()
	if err != nil {
		return err
	}
	jsonText := string(data)

	const chunkSize = 3800
	chunks := splitToChunks(jsonText, chunkSize)
	total := len(chunks)
	if total == 0 {
		return nil
	}

	peer, err := b.ensureLogPeer()
	if err != nil {
		return err
	}

	// delete old fragments
	oldIDs, _ := b.collectDBFragmentsFromHistory()
	if len(oldIDs) > 0 {
		_ = b.deleteMessages(oldIDs)
		time.Sleep(200 * time.Millisecond)
	}

	for i, ch := range chunks {
		header := fmt.Sprintf("📚 DB PART %d/%d\n", i+1, total)
		text := header + ch
		_, err := b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  text,
			RandomID: rand.Int63(),
		})
		if err != nil {
			b.logger.Printf("Failed to send DB fragment %d: %v", i+1, err)
			// continue attempting other fragments
		}
		// small delay
		time.Sleep(120 * time.Millisecond)
	}
	b.logger.Printf("DB backup to channel complete with %d fragments", total)
	return nil
}

// restore DB from channel fragments
func (b *TelegramBot) restoreDBFromChannelMulti() error {
	peer, err := b.ensureLogPeer()
	if err != nil {
		return err
	}

	ids, err := b.collectDBFragmentsFromHistory()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		// create initial empty fragment if none
		initial := "📚 DB PART 1/1\n[]"
		_, err := b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  initial,
			RandomID: rand.Int63(),
		})
		if err != nil {
			return fmt.Errorf("failed to create initial DB message: %w", err)
		}
		ids, _ = b.collectDBFragmentsFromHistory()
		if len(ids) == 0 {
			return nil
		}
	}

	history, err := b.tgClient.API().MessagesGetHistory(b.tgCtx, &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		Limit: 500,
	})
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
	sort.Slice(fragments, func(i, j int) bool { return fragments[i].Index < fragments[j].Index })
	var builder strings.Builder
	for _, f := range fragments {
		builder.WriteString(f.Text)
	}
	combined := builder.String()
	if err := b.importDatabaseFromJSON([]byte(combined)); err != nil {
		return fmt.Errorf("failed to import DB from combined fragments: %w", err)
	}
	b.logger.Printf("Database restored from %d fragments (total bytes: %d)", len(fragments), len(combined))
	return nil
}

// ================= Dump DB admin command =================
func (b *TelegramBot) handleDumpDB(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}
	peer, err := b.ensureLogPeer()
	if err != nil {
		return b.sendReply(ctx, u, "Could not get log channel peer.")
	}

	history, err := b.tgClient.API().MessagesGetHistory(b.tgCtx, &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		Limit: 500,
	})
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
		_, _ = b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:     b.tgCtx.PeerStorage.GetInputPeerById(adminChatID),
			Message:  "📂 DATABASE DUMP\n" + combined,
			RandomID: rand.Int63(),
		})
	} else {
		_, _ = b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:     b.tgCtx.PeerStorage.GetInputPeerById(adminChatID),
			Message:  fmt.Sprintf("Database dump is large (%d bytes). Check the log channel %s for fragments (total parts: %d).", len(combined), logChannelUsername, totalParts),
			RandomID: rand.Int63(),
		})
	}
	return nil
}

// ================= Utilities: reflection extractors =================
func extractMessagesFromHistory(history interface{}) []reflect.Value {
	v := reflect.ValueOf(history)
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	f := v.FieldByName("Messages")
	if f.IsValid() && f.Kind() == reflect.Slice {
		var out []reflect.Value
		for i := 0; i < f.Len(); i++ {
			out = append(out, f.Index(i))
		}
		return out
	}
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

func readMessageTextAndID(rv reflect.Value) (string, int, bool) {
	if !rv.IsValid() {
		return "", 0, false
	}
	if rv.Kind() == reflect.Interface || rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return "", 0, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return "", 0, false
	}
	text := ""
	fm := rv.FieldByName("Message")
	if fm.IsValid() {
		switch fm.Kind() {
		case reflect.String:
			text = fm.String()
		case reflect.Ptr:
			if !fm.IsNil() && fm.Elem().Kind() == reflect.String {
				text = fm.Elem().String()
			}
		}
	}
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
	if text == "" && id == 0 {
		return "", 0, false
	}
	return text, id, true
}

// ================= Log channel helpers & forwarding =================

// ensureLogPeer resolves and caches the log channel peer (by username)
func (b *TelegramBot) ensureLogPeer() (tg.InputPeerClass, error) {
	if b.logPeerInit && b.logChannelPeer != nil {
		return b.logChannelPeer, nil
	}
	// use helper in utils to resolve peer by username (expected signature)
	peer, err := utils.GetLogChannelPeer(b.tgCtx, logChannelUsername)
	if err != nil {
		// fallback: try PeerStorage
		// strip @ and try to parse numeric?
		b.logger.Printf("utils.GetLogChannelPeer failed: %v", err)
		// cannot proceed without peer
		return nil, err
	}
	b.logChannelPeer = peer
	b.logPeerInit = true
	return peer, nil
}

// trySendLog attempts best-effort to send a text to log channel (non-blocking caller)
func (b *TelegramBot) trySendLog(text string) error {
	peer, err := b.ensureLogPeer()
	if err != nil {
		b.logger.Printf("trySendLog: ensureLogPeer failed: %v", err)
		return err
	}
	_, err = b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  text,
		RandomID: rand.Int63(),
	})
	if err != nil {
		b.logger.Printf("trySendLog send failed: %v", err)
	}
	return err
}

// forwardToLogChannelAsync forwards the incoming message to the log channel and replies with user info
func (b *TelegramBot) forwardToLogChannelAsync(ctx *ext.Context, u *ext.Update) error {
	go func() {
		fromChatID := u.EffectiveChat().GetID()
		messageID := u.EffectiveMessage.Message.ID

		peer, err := b.ensureLogPeer()
		if err != nil {
			b.logger.Printf("forwardToLogChannelAsync: ensureLogPeer failed: %v", err)
			return
		}

		// Prepare forward request
		forwardReq := &tg.MessagesForwardMessagesRequest{
			FromPeer: b.tgCtx.PeerStorage.GetInputPeerById(fromChatID),
			ToPeer:   peer,
			ID:       []int{messageID},
			RandomID: []int64{rand.Int63()},
		}

		fwdRes, err := b.tgClient.API().MessagesForwardMessages(b.tgCtx, forwardReq)
		if err != nil {
			b.logger.Printf("Failed to forward message %d from %d to log channel: %v", messageID, fromChatID, err)
			return
		}

		// Extract new message id from forward response (reflection, robust)
		newMsgID := extractNewMessageIDFromForwardResult(fwdRes)
		if newMsgID == 0 {
			// It may still be present in updates; attempt to fetch recent history and find latest message from bot
			b.logger.Printf("Could not extract newMsgID from forward response; attempting fallback history lookup")
			time.Sleep(200 * time.Millisecond)
			// fetch latest history
			history, herr := b.tgClient.API().MessagesGetHistory(b.tgCtx, &tg.MessagesGetHistoryRequest{
				Peer:  peer,
				Limit: 10,
			})
			if herr != nil {
				b.logger.Printf("Fallback history lookup failed: %v", herr)
			} else {
				msgs := extractMessagesFromHistory(history)
				for _, mv := range msgs {
					_, id, ok := readMessageTextAndID(mv)
					if ok {
						// use first found as fallback
						newMsgID = id
						break
					}
				}
			}
		}
		if newMsgID == 0 {
			b.logger.Printf("forwardToLogChannelAsync: could not determine new message id after forward")
		}

		// Build user info and send as a reply to forwarded message
		uinfo, err := b.userRepository.GetUserInfo(fromChatID)
		if err != nil {
			b.logger.Printf("Could not get user info for forwarded message: %v", err)
			return
		}
		username := "N/A"
		if uinfo.Username != "" {
			username = "@" + uinfo.Username
		}
		infoMsg := fmt.Sprintf("Media from user:\nID: %d\nName: %s %s\nUsername: %s",
			uinfo.UserID, uinfo.FirstName, uinfo.LastName, username)

		// Send reply in channel pointing to forwarded message
		_, err = b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  infoMsg,
			RandomID: rand.Int63(),
			ReplyTo: &tg.InputReplyToMessage{
				ReplyToMsgID: newMsgID,
			},
		})
		if err != nil {
			b.logger.Printf("Failed to send user info reply to log channel: %v", err)
			return
		}
	}()
	return nil
}

// extractNewMessageIDFromForwardResult tries to locate a new channel message id inside forward response
func extractNewMessageIDFromForwardResult(res interface{}) int {
	v := reflect.ValueOf(res)
	if !v.IsValid() {
		return 0
	}
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if !v.IsValid() {
		return 0
	}
	// Check for Updates field
	upField := v.FieldByName("Updates")
	if upField.IsValid() && upField.Kind() == reflect.Slice {
		for i := 0; i < upField.Len(); i++ {
			el := upField.Index(i)
			// possibly slice of interface{} or concrete types
			switch el.Kind() {
			case reflect.Interface, reflect.Ptr:
				if el.IsNil() {
					continue
				}
				elm := el.Elem()
				// look for UpdateNewChannelMessage
				if elm.Type().String() == "tg.UpdateNewChannelMessage" || strings.HasSuffix(elm.Type().String(), ".UpdateNewChannelMessage") {
					// find Message field and its ID
					msgField := elm.FieldByName("Message")
					if msgField.IsValid() {
						// message might be pointer to tg.Message
						m := msgField
						if m.Kind() == reflect.Ptr {
							if m.IsNil() {
								continue
							}
							m = m.Elem()
						}
						// id field name may be ID or Id
						idField := m.FieldByName("ID")
						if !idField.IsValid() {
							idField = m.FieldByName("Id")
						}
						if idField.IsValid() && (idField.Kind() == reflect.Int || idField.Kind() == reflect.Int32 || idField.Kind() == reflect.Int64) {
							return int(idField.Int())
						}
					}
				}
			default:
				// skip
			}
		}
	}
	// fallback zero
	return 0
}

// ================= Misc =================
func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error { return nil }
