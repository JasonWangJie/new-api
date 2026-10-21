package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const CanvasClientProofHeader = "X-Infinite-Canvas-Client"
const CanvasGrantHeader = "X-Infinite-Canvas-Grant"
const CanvasGrantContextKey = "infinite_canvas_grant"
const CanvasCodeTTL = 60 * time.Second
const CanvasGrantTTL = 8 * time.Hour
const CanvasIdleTTL = 30 * time.Minute

var ErrCanvasAuthorization = errors.New("Infinite Canvas authorization is invalid or expired")
var ErrCanvasUnavailable = errors.New("Infinite Canvas authorization is unavailable")

type CanvasAuthorizationError struct{ Reason string }

func (e *CanvasAuthorizationError) Error() string { return ErrCanvasAuthorization.Error() }
func (e *CanvasAuthorizationError) Unwrap() error { return ErrCanvasAuthorization }
func CanvasInactiveReason(err error) string {
	var inactive *CanvasAuthorizationError
	if errors.As(err, &inactive) {
		return inactive.Reason
	}
	return "grant_revoked"
}

type CanvasUser struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

type CanvasGrantState struct {
	ID         string       `json:"grant_id"`
	Identity   AuthIdentity `json:"identity"`
	ClientID   string       `json:"client_id"`
	Subject    string       `json:"subject"`
	ConfigHash string       `json:"config_hash"`
	TokenID    int          `json:"token_id"`
	KeyHash    string       `json:"key_hash"`
	ExpiresAt  int64        `json:"expires_at"`
}

type canvasAuthorizationCode struct {
	Grant       CanvasGrantState `json:"grant"`
	RedirectURI string           `json:"redirect_uri"`
	Challenge   string           `json:"challenge"`
	ExpiresAt   int64            `json:"expires_at"`
}

type CanvasExchange struct {
	GrantID   string     `json:"grant_id"`
	APIKey    string     `json:"api_key"`
	User      CanvasUser `json:"user"`
	ExpiresAt int64      `json:"expires_at"`
}

func canvasDigest(value string) string {
	return common.GenerateHMACWithKey([]byte("infinite-canvas-v1:"+common.SessionSecret), value)
}

func canvasConfigHash(settings system_setting.InfiniteCanvasSettings) string {
	return canvasDigest(settings.ClientID + "\x00" + settings.URL + "\x00" + system_setting.CanvasClientSecret())
}

func canvasReady() (system_setting.InfiniteCanvasSettings, error) {
	settings := system_setting.InfiniteCanvasSettingsSnapshot()
	if !settings.Enabled || len(system_setting.CanvasClientSecret()) < 32 || !common.RedisEnabled || common.RDB == nil {
		return settings, ErrCanvasUnavailable
	}
	if _, err := system_setting.CanvasCallbackURL(settings); err != nil {
		return settings, ErrCanvasUnavailable
	}
	return settings, nil
}

// AuthenticateCanvasClient is only for confidential server-to-server requests.
func AuthenticateCanvasClient(header string) bool {
	settings, err := canvasReady()
	if err != nil {
		return false
	}
	r := &http.Request{Header: http.Header{"Authorization": []string{header}}}
	id, secret, ok := r.BasicAuth()
	actualID, expectedID := sha256.Sum256([]byte(id)), sha256.Sum256([]byte(settings.ClientID))
	actualSecret, expectedSecret := sha256.Sum256([]byte(secret)), sha256.Sum256([]byte(system_setting.CanvasClientSecret()))
	return ok && subtle.ConstantTimeCompare(actualID[:], expectedID[:]) == 1 && subtle.ConstantTimeCompare(actualSecret[:], expectedSecret[:]) == 1
}

func canvasIdentityUser(ctx context.Context, identity AuthIdentity) (*model.User, error) {
	if identity.UserID <= 0 || identity.SessionID == "" || identity.UserAuthVersion <= 0 || identity.SessionVersion <= 0 {
		return nil, ErrCanvasAuthorization
	}
	var user model.User
	if err := model.DB.WithContext(ctx).First(&user, identity.UserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &CanvasAuthorizationError{Reason: "user_deleted"}
		}
		return nil, ErrCanvasUnavailable
	}
	if user.Status != common.UserStatusEnabled {
		return nil, &CanvasAuthorizationError{Reason: "user_disabled"}
	}
	if user.AuthVersion != identity.UserAuthVersion {
		return nil, &CanvasAuthorizationError{Reason: "security_changed"}
	}
	var session model.UserSession
	if err := model.DB.WithContext(ctx).Where("sid = ? AND user_id = ?", identity.SessionID, identity.UserID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &CanvasAuthorizationError{Reason: "session_revoked"}
		}
		return nil, ErrCanvasUnavailable
	}
	if session.UserAuthVersion != identity.UserAuthVersion {
		return nil, &CanvasAuthorizationError{Reason: "security_changed"}
	}
	if session.Status != model.UserSessionStatusActive || session.RevokedAt != 0 || session.Version != identity.SessionVersion {
		return nil, &CanvasAuthorizationError{Reason: "session_revoked"}
	}
	if session.ExpiresAt <= time.Now().Unix() {
		return nil, &CanvasAuthorizationError{Reason: "session_expired"}
	}
	return &user, nil
}

func AuthorizeCanvas(ctx context.Context, identity AuthIdentity, clientID, redirectURI, state, challenge, method string) (string, error) {
	settings, err := canvasReady()
	if err != nil {
		return "", err
	}
	callback, _ := system_setting.CanvasCallbackURL(settings)
	decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(challenge)
	if clientID != settings.ClientID || redirectURI != callback || len(state) < 16 || len(state) > 512 || strings.ContainsAny(state, "\r\n") || method != "S256" || decodeErr != nil || len(decoded) != sha256.Size {
		return "", ErrCanvasAuthorization
	}
	if _, err := canvasIdentityUser(ctx, identity); err != nil {
		return "", err
	}
	var parent model.UserSession
	if err := model.DB.WithContext(ctx).Where("sid = ? AND user_id = ?", identity.SessionID, identity.UserID).First(&parent).Error; err != nil {
		return "", ErrCanvasAuthorization
	}
	code, err := common.GenerateRandomCharsKey(64)
	if err != nil {
		return "", err
	}
	now := time.Now()
	grant := CanvasGrantState{ID: "icg_" + common.GetUUID(), Identity: identity, ClientID: clientID, ConfigHash: canvasConfigHash(settings), ExpiresAt: min(now.Add(CanvasGrantTTL).Unix(), parent.ExpiresAt)}
	payload, err := common.Marshal(canvasAuthorizationCode{Grant: grant, RedirectURI: callback, Challenge: challenge, ExpiresAt: now.Add(CanvasCodeTTL).Unix()})
	if err != nil {
		return "", err
	}
	key := "canvas:code:" + canvasDigest(code)
	_, err = common.RDB.Eval(ctx, `redis.call('HSET',KEYS[1],'payload',ARGV[1],'status','pending','grant_id',ARGV[2]); redis.call('EXPIREAT',KEYS[1],ARGV[3]); return 1`, []string{key}, string(payload), grant.ID, grant.ExpiresAt+60).Result()
	if err != nil {
		return "", ErrCanvasUnavailable
	}
	logger.LogInfo(ctx, fmt.Sprintf("canvas_authorization event=code_issued client_id=%q user_id=%d", clientID, identity.UserID))
	u, _ := url.Parse(callback)
	query := u.Query()
	query.Set("code", code)
	query.Set("state", state)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// ExchangeCanvasCode claims the code before issuing a SQL token. Replays also
// tombstone an in-progress issuance, so the issuer cannot later activate it.
func ExchangeCanvasCode(ctx context.Context, code, redirectURI, verifier string) (*CanvasExchange, error) {
	settings, err := canvasReady()
	if err != nil {
		return nil, err
	}
	if len(code) != 64 || len(verifier) < 43 || len(verifier) > 128 {
		return nil, ErrCanvasAuthorization
	}
	for _, ch := range verifier {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._~", ch)) {
			return nil, ErrCanvasAuthorization
		}
	}
	codeKey := "canvas:code:" + canvasDigest(code)
	raw, err := common.RDB.HGet(ctx, codeKey, "payload").Result()
	if err != nil {
		return nil, ErrCanvasAuthorization
	}
	var authorization canvasAuthorizationCode
	if common.UnmarshalJsonStr(raw, &authorization) != nil {
		return nil, ErrCanvasAuthorization
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	grant := authorization.Grant
	if grant.ClientID != settings.ClientID || grant.ConfigHash != canvasConfigHash(settings) || redirectURI != authorization.RedirectURI || challenge != authorization.Challenge {
		return nil, ErrCanvasAuthorization
	}
	grantKey := "canvas:grant:" + grant.ID
	const claim = `
if redis.call('HGET',KEYS[1],'payload') ~= ARGV[1] then return 0 end
if redis.call('HGET',KEYS[1],'status') ~= 'pending' then
 redis.call('HSET',KEYS[1],'status','replayed')
 redis.call('HSET',KEYS[2],'status','revoked')
 redis.call('EXPIREAT',KEYS[2],ARGV[4]); return -1
end
if tonumber(ARGV[2]) >= tonumber(ARGV[3]) then return 0 end
redis.call('HSET',KEYS[1],'status','consumed')
redis.call('HSET',KEYS[2],'status','issuing','last_active',ARGV[2])
redis.call('EXPIREAT',KEYS[2],ARGV[4]); return 1`
	claimed, err := common.RDB.Eval(ctx, claim, []string{codeKey, grantKey}, raw, time.Now().Unix(), authorization.ExpiresAt, grant.ExpiresAt).Int()
	if err != nil {
		return nil, ErrCanvasUnavailable
	}
	if claimed != 1 {
		if claimed == -1 {
			logger.LogWarn(ctx, "canvas_authorization event=code_replay client_id=%q user_id=%d", grant.ClientID, grant.Identity.UserID)
			_ = RevokeCanvasGrant(ctx, grant.ID)
		}
		return nil, ErrCanvasAuthorization
	}
	user, err := canvasIdentityUser(ctx, grant.Identity)
	if err != nil {
		_ = RevokeCanvasGrant(ctx, grant.ID)
		return nil, err
	}
	secret, err := common.GenerateKey()
	if err != nil {
		_ = RevokeCanvasGrant(ctx, grant.ID)
		return nil, err
	}
	token := model.Token{UserId: user.Id, Key: "ic_" + secret, Name: "Infinite Canvas", Status: common.TokenStatusEnabled, CreatedTime: time.Now().Unix(), AccessedTime: time.Now().Unix(), ExpiredTime: grant.ExpiresAt, UnlimitedQuota: true}
	err = model.DB.WithContext(ctx).Session(&gorm.Session{Logger: gormlogger.Discard}).Transaction(func(tx *gorm.DB) error {
		if err := model.ValidateAuthSessionWithTx(tx, grant.Identity); err != nil {
			return err
		}
		var owner model.User
		if err := tx.First(&owner, user.Id).Error; err != nil {
			return err
		}
		if owner.CanvasSubject == nil {
			subject := "ics_" + common.GetUUID()
			result := tx.Model(&model.User{}).Where("id = ? AND canvas_subject IS NULL", user.Id).Update("canvas_subject", subject)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrCanvasAuthorization
			}
			owner.CanvasSubject = &subject
		}
		if *owner.CanvasSubject == "" {
			return ErrCanvasAuthorization
		}
		grant.Subject = *owner.CanvasSubject
		if err := tx.Create(&token).Error; err != nil {
			return err
		}
		return tx.Create(&model.CanvasGrant{ID: grant.ID, TokenID: token.Id, UserID: user.Id, ClientID: grant.ClientID, Subject: grant.Subject, CreatedAt: time.Now().Unix(), ExpiresAt: grant.ExpiresAt}).Error
	})
	if err != nil {
		_ = RevokeCanvasGrant(ctx, grant.ID)
		return nil, ErrCanvasUnavailable
	}
	if common.RedisEnabled {
		if err := model.RefreshCanvasSubjectCache(user.Id, grant.Subject); err != nil {
			_ = RevokeCanvasGrant(ctx, grant.ID)
			return nil, ErrCanvasUnavailable
		}
	}
	grant.TokenID, grant.KeyHash = token.Id, canvasDigest(token.Key)
	payload, err := common.Marshal(grant)
	if err != nil {
		_ = RevokeCanvasGrant(ctx, grant.ID)
		return nil, ErrCanvasUnavailable
	}
	const activate = `
if redis.call('HGET',KEYS[1],'status') ~= 'consumed' or redis.call('HGET',KEYS[2],'status') ~= 'issuing' then return 0 end
redis.call('HSET',KEYS[2],'status','active','payload',ARGV[1]); return 1`
	activated, err := common.RDB.Eval(ctx, activate, []string{codeKey, grantKey}, string(payload)).Int()
	if err != nil || activated != 1 {
		_ = RevokeCanvasGrant(ctx, grant.ID)
		return nil, ErrCanvasAuthorization
	}
	logger.LogInfo(ctx, fmt.Sprintf("canvas_authorization event=grant_issued client_id=%q user_id=%d token_id=%d", grant.ClientID, user.Id, token.Id))
	return &CanvasExchange{GrantID: grant.ID, APIKey: "sk-" + token.Key, User: CanvasUser{ID: user.Id, Subject: grant.Subject, Username: user.Username, DisplayName: user.DisplayName}, ExpiresAt: grant.ExpiresAt}, nil
}

func GetCanvasGrant(ctx context.Context, id string, activity bool) (*CanvasGrantState, *model.User, error) {
	settings, err := canvasReady()
	if err != nil {
		return nil, nil, err
	}
	if len(id) != 36 || !strings.HasPrefix(id, "icg_") {
		return nil, nil, ErrCanvasAuthorization
	}
	values, err := common.RDB.HGetAll(ctx, "canvas:grant:"+id).Result()
	if err != nil {
		return nil, nil, ErrCanvasUnavailable
	}
	var grant CanvasGrantState
	if values["status"] != "active" {
		if values["reason"] == "idle_expired" {
			return nil, nil, &CanvasAuthorizationError{Reason: "idle_expired"}
		}
		var record model.CanvasGrant
		if model.DB.WithContext(ctx).Where("id = ?", id).Take(&record).Error == nil && record.ExpiresAt <= time.Now().Unix() {
			return nil, nil, &CanvasAuthorizationError{Reason: "grant_expired"}
		}
		return nil, nil, &CanvasAuthorizationError{Reason: "grant_revoked"}
	}
	if common.UnmarshalJsonStr(values["payload"], &grant) != nil || grant.ID != id || grant.ClientID != settings.ClientID || grant.ConfigHash != canvasConfigHash(settings) {
		return nil, nil, ErrCanvasAuthorization
	}
	if grant.ExpiresAt <= time.Now().Unix() {
		return nil, nil, &CanvasAuthorizationError{Reason: "grant_expired"}
	}
	var record model.CanvasGrant
	if err := model.DB.WithContext(ctx).Where("id = ? AND token_id = ? AND user_id = ? AND client_id = ? AND subject = ? AND revoked_at = 0", id, grant.TokenID, grant.Identity.UserID, grant.ClientID, grant.Subject).Take(&record).Error; err != nil {
		return nil, nil, ErrCanvasAuthorization
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).First(&token, grant.TokenID).Error; err != nil || token.UserId != grant.Identity.UserID || !model.IsCanvasTokenKey(token.Key) || canvasDigest(token.Key) != grant.KeyHash || token.Status != common.TokenStatusEnabled || token.ExpiredTime <= time.Now().Unix() {
		return nil, nil, ErrCanvasAuthorization
	}
	// Read authoritative state: a Redis failure must never preserve stale parent privileges.
	user, err := canvasIdentityUser(ctx, grant.Identity)
	if err != nil {
		return nil, nil, err
	}
	if user.CanvasSubject == nil || *user.CanvasSubject != grant.Subject {
		return nil, nil, &CanvasAuthorizationError{Reason: "security_changed"}
	}
	const checkActivity = `
if redis.call('HGET',KEYS[1],'status') ~= 'active' then return 0 end
local last = tonumber(redis.call('HGET',KEYS[1],'last_active') or '0')
if tonumber(ARGV[1])-last >= tonumber(ARGV[2]) then redis.call('HSET',KEYS[1],'status','revoked','reason','idle_expired'); return -1 end
if ARGV[3] == '1' then redis.call('HSET',KEYS[1],'last_active',ARGV[1]) end
return 1`
	touch := "0"
	if activity {
		touch = "1"
	}
	active, err := common.RDB.Eval(ctx, checkActivity, []string{"canvas:grant:" + id}, time.Now().Unix(), int64(CanvasIdleTTL/time.Second), touch).Int()
	if err != nil {
		return nil, nil, ErrCanvasUnavailable
	}
	if active != 1 {
		if active == -1 {
			return nil, nil, &CanvasAuthorizationError{Reason: "idle_expired"}
		}
		return nil, nil, ErrCanvasAuthorization
	}
	return &grant, user, nil
}

func RevokeCanvasGrant(ctx context.Context, id string) error {
	if len(id) != 36 || !strings.HasPrefix(id, "icg_") {
		return ErrCanvasAuthorization
	}
	if !common.RedisEnabled || common.RDB == nil {
		return ErrCanvasUnavailable
	}
	// Keep a deny marker until maximum issuance expiry, including issuance races.
	if _, err := common.RDB.Eval(ctx, `redis.call('HSET',KEYS[1],'status','revoked'); redis.call('EXPIRE',KEYS[1],ARGV[1]); return 1`, []string{"canvas:grant:" + id}, int64(CanvasGrantTTL/time.Second)).Result(); err != nil {
		return ErrCanvasUnavailable
	}
	return model.DB.WithContext(ctx).Model(&model.CanvasGrant{}).Where("id = ? AND revoked_at = 0", id).Update("revoked_at", time.Now().Unix()).Error
}

func CanvasPathAllowed(method, path string) bool {
	if method == http.MethodGet && path == "/v1/models" {
		return true
	}
	if method == http.MethodPost {
		switch path {
		case "/v1/responses", "/v1/chat/completions", "/v1/images/generations", "/v1/images/edits", "/v1/videos", "/v1/videos/edits", "/v1/videos/extensions", "/v1/audio/speech", "/v1/images/generations_async", "/v1/images/edits_async", "/v1/videos/generations_async", "/v1/videos/edits_async", "/v1/videos/extensions_async", "/v1/uploads/images_sc":
			return true
		}
		if strings.HasPrefix(path, "/v1beta/models/") && (strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")) {
			return true
		}
	}
	if method == http.MethodGet || method == http.MethodHead {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) == 3 && parts[0] == "v1" && parts[1] == "videos" && parts[2] != "" {
			return true
		}
		if len(parts) == 4 && parts[0] == "v1" && parts[1] == "videos" && parts[2] != "" && parts[3] == "content" {
			return true
		}
		if len(parts) == 4 && parts[0] == "v1" && (parts[1] == "images" || parts[1] == "media") && parts[2] == "tasks_async" && parts[3] != "stats" {
			return true
		}
	}
	return false
}

func ValidateCanvasCredential(ctx context.Context, key, proof, method, path string) (*model.Token, *model.UserBase, *CanvasGrantState, error) {
	if !AuthenticateCanvasClient(proof) || !CanvasPathAllowed(method, path) {
		return nil, nil, nil, ErrCanvasAuthorization
	}
	var token model.Token
	if err := model.DB.WithContext(ctx).Session(&gorm.Session{Logger: gormlogger.Discard}).Where(&model.Token{Key: key}).First(&token).Error; err != nil {
		return nil, nil, nil, ErrCanvasAuthorization
	}
	if subtle.ConstantTimeCompare([]byte(token.Key), []byte(key)) != 1 || token.Status != common.TokenStatusEnabled || token.ExpiredTime <= time.Now().Unix() {
		return nil, nil, nil, ErrCanvasAuthorization
	}
	var record model.CanvasGrant
	if err := model.DB.WithContext(ctx).Where("token_id = ? AND user_id = ?", token.Id, token.UserId).Take(&record).Error; err != nil {
		return nil, nil, nil, ErrCanvasAuthorization
	}
	grant, user, err := GetCanvasGrant(ctx, record.ID, method == http.MethodPost)
	if err != nil {
		return nil, nil, nil, err
	}
	if grant.KeyHash != canvasDigest(key) || grant.TokenID != token.Id || grant.Identity.UserID != token.UserId {
		return nil, nil, nil, ErrCanvasAuthorization
	}
	return &token, user.ToBaseUser(), grant, nil
}

// CanvasImageObjectManaged follows durable provenance rather than assuming an
// owner column exists on the shared image object table.
func CanvasImageObjectManaged(ctx context.Context, object model.ImageStorageObject) (bool, error) {
	var count int64
	if err := model.DB.WithContext(ctx).Table("image_upload_intents").Joins("JOIN canvas_grants ON canvas_grants.token_id = image_upload_intents.token_id").Where("image_upload_intents.profile_id = ? AND image_upload_intents.object_key = ?", object.ProfileId, object.ObjectKey).Count(&count).Error; err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	err := model.DB.WithContext(ctx).Table("async_image_results").Joins("JOIN async_image_tasks ON async_image_tasks.task_id = async_image_results.task_id").Joins("JOIN canvas_grants ON canvas_grants.token_id = async_image_tasks.token_id").Where("async_image_results.object_id = ?", object.ObjectId).Count(&count).Error
	return count > 0, err
}

func CanvasContentPath(kind, taskID, artifact string) string {
	return "/api/integrations/canvas/history/" + kind + "/" + url.PathEscape(taskID) + "/content/" + url.PathEscape(artifact)
}

// CanvasTaskAccess retains ordinary task policy and additionally binds managed
// reads and origin-task edits to immutable application/account provenance.
func CanvasTaskAccess(c *gin.Context, task *model.Task) bool {
	if task == nil {
		return false
	}
	value, exists := c.Get(CanvasGrantContextKey)
	if !exists {
		subject, err := model.CanvasBillingSubject(model.DB.WithContext(c.Request.Context()), task.PrivateData.TokenId, task.UserId)
		if err != nil {
			return false
		}
		if subject == "" {
			return true
		}
		var owner model.User
		return task.UserId == c.GetInt("id") && model.DB.WithContext(c.Request.Context()).First(&owner, task.UserId).Error == nil && owner.CanvasSubject != nil && *owner.CanvasSubject == subject
	}
	grant, ok := value.(*CanvasGrantState)
	if !ok || grant == nil || task == nil || task.UserId != grant.Identity.UserID {
		return false
	}
	var count int64
	err := model.DB.WithContext(c.Request.Context()).Model(&model.CanvasGrant{}).Where("token_id = ? AND user_id = ? AND client_id = ? AND subject = ?", task.PrivateData.TokenId, task.UserId, grant.ClientID, grant.Subject).Count(&count).Error
	return err == nil && count == 1
}

// CanvasInputURL is a narrowly scoped, expiring input capability for upstream
// image fetchers. It never grants browser session or arbitrary object access.
func CanvasInputURL(base, inputID string, grant *CanvasGrantState, seconds int) string {
	expires := min(time.Now().Add(time.Duration(seconds)*time.Second).Unix(), grant.ExpiresAt)
	q := url.Values{"grant": {grant.ID}, "expires": {strconv.FormatInt(expires, 10)}}
	q.Set("signature", canvasDigest("input\x00"+inputID+"\x00"+grant.ID+"\x00"+q.Get("expires")))
	return strings.TrimRight(base, "/") + "/api/integrations/canvas/inputs/" + url.PathEscape(inputID) + "/content?" + q.Encode()
}

func ValidateCanvasInputAccess(ctx context.Context, inputID, grantID, expires, signature string) (*CanvasGrantState, error) {
	timestamp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || timestamp <= time.Now().Unix() {
		return nil, ErrCanvasAuthorization
	}
	expected := canvasDigest("input\x00" + inputID + "\x00" + grantID + "\x00" + expires)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) != 1 {
		return nil, ErrCanvasAuthorization
	}
	grant, _, err := GetCanvasGrant(ctx, grantID, false)
	if err != nil || timestamp > grant.ExpiresAt {
		return nil, ErrCanvasAuthorization
	}
	return grant, nil
}
