package controller

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const storedResultTestBase = "https://results.example.com"
const storedResultTestPath = storedResultTestBase + "/v1/media/files/"

const (
	storedResultS3Chunked int32 = iota + 1
	storedResultS3Truncated
)

func newStoredResultFixture(t *testing.T) (*canvasFixture, model.Token) {
	t.Helper()
	f := newCanvasFixture(t)
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
	previousServer, previousTaskBase, previousSecret := system_setting.ServerAddress, system_setting.TaskPublicAddress, common.CryptoSecret
	system_setting.ServerAddress, system_setting.TaskPublicAddress = storedResultTestBase, "https://references.example.com"
	common.CryptoSecret = "direct-result-test-secret"
	t.Cleanup(func() {
		system_setting.ServerAddress, system_setting.TaskPublicAddress, common.CryptoSecret = previousServer, previousTaskBase, previousSecret
	})
	token := model.Token{UserId: f.user.Id, Key: "ordinary" + common.GetUUID(), Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true, Group: "default"}
	require.NoError(t, f.db.Create(&token).Error)
	return f, token
}

func storedResultImage(t *testing.T, profile model.ImageStorageProfile, task model.AsyncImageTask, index int, mime string, data []byte) model.ImageStorageObject {
	t.Helper()
	payload := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	image, err := service.DownloadImageOutput(t.Context(), payload, service.DefaultImageRuntimeConfig())
	require.NoError(t, err)
	require.Equal(t, mime, image.ContentType)
	require.Equal(t, data, image.Data)
	intent := model.ImageUploadIntent{
		IntentKey: common.GetUUID(), TaskId: task.TaskId, TokenId: task.TokenId, UserId: task.UserId, ProfileId: profile.ProfileId, Class: profile.Class,
		ObjectKey: service.ImageResultObjectKey(task, index, mime), Status: "pending", Checksum: image.Checksum, ByteSize: int64(len(image.Data)),
	}
	object, err := service.StoreImageIntent(t.Context(), intent, image, task.ExpiresAt)
	require.NoError(t, err)
	return object
}

func storedResultS3Profile(t *testing.T, f *canvasFixture, getMode *atomic.Int32) (model.ImageStorageProfile, *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	var reads atomic.Int32
	objects := map[string][]byte{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=result-test/"))
		assert.Empty(t, r.URL.Query().Get("X-Amz-Signature"))
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			objects[r.URL.Path] = body
			w.Header().Set("ETag", "\"private-s3\"")
		case http.MethodGet, http.MethodHead:
			reads.Add(1)
			body, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet {
				switch getMode.Load() {
				case storedResultS3Chunked:
					w.WriteHeader(http.StatusOK)
					if !assert.NoError(t, http.NewResponseController(w).Flush()) {
						return
					}
					_, _ = w.Write(body)
					return
				case storedResultS3Truncated:
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					_, _ = w.Write(body[:len(body)/2])
					return
				}
			}
			http.ServeContent(w, r, r.URL.Path, time.Time{}, bytes.NewReader(body))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	profile := model.ImageStorageProfile{ProfileId: "private-result-s3", Class: "temporary", Backend: "s3", Bucket: "private-results", Region: "us-east-1", Endpoint: server.URL, PathStyle: true, Active: true}
	credential, err := common.Marshal(service.ImageStorageCredentials{AccessKey: "result-test", SecretKey: "synthetic-s3-secret"})
	require.NoError(t, err)
	profile.CredentialCipher, err = service.EncryptImagePayload(credential, "storage:"+profile.ProfileId)
	require.NoError(t, err)
	require.NoError(t, f.db.Create(&profile).Error)
	return profile, &reads
}

func queryStoredResultFiles(t *testing.T, f *canvasFixture, query string, token model.Token) []string {
	t.Helper()
	response := f.request(http.MethodGet, query, "", "Bearer "+token.Key, "", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var manifest struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &manifest))
	links := make([]string, len(manifest.Data))
	for i, item := range manifest.Data {
		links[i] = item.URL
	}
	return links
}

func TestStoredResultFilePerImageExternalDelivery(t *testing.T) {
	f, token := newStoredResultFixture(t)
	profile := model.ImageStorageProfile{ProfileId: "external-delivery-local", Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}
	require.NoError(t, f.db.Create(&profile).Error)
	task := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: f.user.Id, TokenId: token.Id, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ImageCount: 2, ResultCount: 2, CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, f.db.Create(&task).Error)
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 2))))
	objects := make([]model.ImageStorageObject, 2)
	for index := range objects {
		objects[index] = storedResultImage(t, profile, task, index, "image/png", data.Bytes())
		require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ImageIndex: index, ObjectId: objects[index].ObjectId}).Error)
	}
	oldLink, err := service.ImageResultFileURL(t.Context(), objects[0], storedResultTestBase)
	require.NoError(t, err)
	hosted := "https://images.example.com/i/generated.png"
	require.NoError(t, f.db.Model(&objects[0]).Updates(map[string]any{"delivery_source": "hosted", "delivery_url": hosted}).Error)
	query := "/v1/images/tasks_async/" + task.TaskId
	links := queryStoredResultFiles(t, f, query, token)
	require.Len(t, links, 2)
	assert.Equal(t, hosted, links[0])
	assert.Contains(t, links[1], "/v1/media/files/img_")
	parsed, err := url.Parse(oldLink)
	require.NoError(t, err)
	router := gin.New()
	router.GET("/v1/media/files/:filename", StoredResultFileContent)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, parsed.Path, nil))
	require.Equal(t, http.StatusTemporaryRedirect, response.Code)
	assert.Equal(t, hosted, response.Header().Get("Location"))
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	// A stale public filename must not redirect after the underlying result expires.
	require.NoError(t, f.db.Model(&objects[0]).Update("expires_at", time.Now().Unix()-1).Error)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, parsed.Path, nil))
	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.Empty(t, response.Header().Get("Location"))
}

func TestStoredResultFileImageDetails(t *testing.T) {
	f, token := newStoredResultFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.Channel{}))
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	for _, backend := range []string{"local", "s3"} {
		t.Run(backend, func(t *testing.T) {
			profile := model.ImageStorageProfile{ProfileId: "detail-local", Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}
			if backend == "s3" {
				var getMode atomic.Int32
				profile, _ = storedResultS3Profile(t, f, &getMode)
			} else {
				require.NoError(t, f.db.Create(&profile).Error)
			}
			task := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: f.user.Id, TokenId: token.Id, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: 1, CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(24 * time.Hour).Unix()}
			require.NoError(t, f.db.Create(&task).Error)
			object := storedResultImage(t, profile, task, 0, "image/png", data.Bytes())
			require.NoError(t, f.db.Model(&object).Update("expires_at", task.ExpiresAt-3600).Error)
			require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ObjectId: object.ObjectId}).Error)
			var link string
			for _, surface := range []struct {
				name, accept string
				userID       int
				admin, view  bool
				status       int
			}{
				{name: "other user detail", userID: task.UserId + 1, status: http.StatusNotFound},
				{name: "other user view", userID: task.UserId + 1, view: true, status: http.StatusNotFound},
				{name: "owner media detail issues file URL", userID: task.UserId, status: http.StatusOK},
				{name: "administrator media detail reuses file URL", userID: task.UserId + 1, admin: true, status: http.StatusOK},
				{name: "owner JSON view reuses file URL", userID: task.UserId, view: true, accept: "application/json", status: http.StatusOK},
				{name: "administrator JSON view reuses file URL", userID: task.UserId + 1, admin: true, view: true, accept: "application/json", status: http.StatusOK},
				{name: "owner preview redirects to file URL", userID: task.UserId, view: true, status: http.StatusTemporaryRedirect},
			} {
				t.Run(surface.name, func(t *testing.T) {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Set("id", surface.userID)
					c.Params = gin.Params{{Key: "task_id", Value: task.TaskId}, {Key: "image_index", Value: "0"}}
					c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
					c.Request.Header.Set("Accept", surface.accept)
					if surface.view {
						ViewImageTaskResult(c, surface.admin)
					} else {
						GetMediaTask(c, surface.admin)
					}
					require.Equal(t, surface.status, recorder.Code, recorder.Body.String())
					if surface.status == http.StatusNotFound {
						var current model.ImageStorageObject
						require.NoError(t, f.db.First(&current, object.Id).Error)
						assert.Nil(t, current.ResultFileId, "unauthorized reads must not issue a public filename")
						return
					}
					if surface.status == http.StatusTemporaryRedirect {
						assert.Equal(t, link, recorder.Header().Get("Location"))
						return
					}
					var response struct {
						Data struct {
							URL       string `json:"url"`
							ExpiresAt int64  `json:"expires_at"`
							Results   []struct {
								URL string `json:"url"`
							} `json:"results"`
						} `json:"data"`
					}
					require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
					actual := response.Data.URL
					if !surface.view {
						require.Len(t, response.Data.Results, 1)
						actual = response.Data.Results[0].URL
					} else {
						assert.Equal(t, min(task.ExpiresAt, object.ExpiresAt), response.Data.ExpiresAt)
					}
					if link == "" {
						require.Regexp(t, "^"+regexp.QuoteMeta(storedResultTestPath)+"img_[0-9a-f]{32}\\.png$", actual)
						link = actual
					}
					assert.Equal(t, link, actual)
				})
			}
			require.NotEmpty(t, link)
			assert.Equal(t, []string{link}, queryStoredResultFiles(t, f, "/v1/images/tasks_async/"+task.TaskId, token))
			content := f.request(http.MethodGet, link, "", "", "", "")
			require.Equal(t, http.StatusOK, content.Code, content.Body.String())
			assert.Equal(t, data.Bytes(), content.Body.Bytes())
		})
	}
}

func TestStoredResultFileImageLifecycle(t *testing.T) {
	f, token := newStoredResultFixture(t)
	gateway := httptest.NewServer(f.router)
	t.Cleanup(gateway.Close)
	imageSize := image.Rect(0, 0, 3, 2)
	raster := image.NewRGBA(imageSize)
	raster.Set(0, 0, color.RGBA{R: 255, A: 255})
	var pngData, jpegData, gifData bytes.Buffer
	require.NoError(t, png.Encode(&pngData, raster))
	require.NoError(t, jpeg.Encode(&jpegData, raster, nil))
	palette := color.Palette{color.Black, color.White}
	first, second := image.NewPaletted(imageSize, palette), image.NewPaletted(imageSize, palette)
	second.SetColorIndex(0, 0, 1)
	require.NoError(t, gif.EncodeAll(&gifData, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 20}, LoopCount: 0}))
	webpData, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	require.NoError(t, err)
	svgData := []byte("<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"3\" height=\"2\"><image width=\"3\" height=\"2\" href=\"data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegData.Bytes()) + "\"/></svg>")
	formats := []struct {
		mime, extension string
		data            []byte
	}{
		{"image/png", ".png", pngData.Bytes()}, {"image/jpeg", ".jpg", jpegData.Bytes()}, {"image/webp", ".webp", webpData},
		{"image/gif", ".gif", gifData.Bytes()}, {"image/svg+xml", ".svg", svgData},
	}
	otherToken := token
	otherToken.Id, otherToken.Key = 0, "ordinary"+common.GetUUID()
	require.NoError(t, f.db.Create(&otherToken).Error)
	for _, backend := range []string{"local", "s3"} {
		t.Run(backend, func(t *testing.T) {
			var profile model.ImageStorageProfile
			var s3Reads *atomic.Int32
			var s3GetMode atomic.Int32
			if backend == "local" {
				profile = model.ImageStorageProfile{ProfileId: "plain-results-local", Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}
				require.NoError(t, f.db.Create(&profile).Error)
			} else {
				profile, s3Reads = storedResultS3Profile(t, f, &s3GetMode)
			}
			task := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: f.user.Id, TokenId: token.Id, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: len(formats), CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}
			require.NoError(t, f.db.Create(&task).Error)
			objects := make([]model.ImageStorageObject, len(formats))
			for i, format := range formats {
				objects[i] = storedResultImage(t, profile, task, i, format.mime, format.data)
				require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ImageIndex: i, ObjectId: objects[i].ObjectId}).Error)
			}
			query := "/v1/images/tasks_async/" + task.TaskId
			assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, query, "", "Bearer "+otherToken.Key, "", "").Code)
			links := queryStoredResultFiles(t, f, query, token)
			require.Len(t, links, len(formats))
			assert.Equal(t, links, queryStoredResultFiles(t, f, query, token))
			for i, format := range formats {
				t.Run(format.extension, func(t *testing.T) {
					link := links[i]
					require.Regexp(t, "^"+regexp.QuoteMeta(storedResultTestPath)+"img_[0-9a-f]{32}"+regexp.QuoteMeta(format.extension)+"$", link)
					assert.NotContains(t, link, objects[i].ObjectId)
					filename := strings.TrimPrefix(link, storedResultTestPath)
					for _, method := range []string{http.MethodGet, http.MethodHead} {
						request, err := http.NewRequest(method, gateway.URL+strings.TrimPrefix(link, storedResultTestBase), nil)
						require.NoError(t, err)
						result, err := gateway.Client().Do(request)
						require.NoError(t, err)
						body, err := io.ReadAll(result.Body)
						require.NoError(t, result.Body.Close())
						require.NoError(t, err)
						require.Equal(t, http.StatusOK, result.StatusCode, string(body))
						assert.Equal(t, format.mime, result.Header.Get("Content-Type"))
						assert.Equal(t, strconv.Itoa(len(format.data)), result.Header.Get("Content-Length"))
						assert.Equal(t, `inline; filename="`+filename+`"`, result.Header.Get("Content-Disposition"))
						assert.Empty(t, result.Header.Get("Location"))
						assert.Equal(t, "nosniff", result.Header.Get("X-Content-Type-Options"))
						if format.extension == ".svg" {
							assert.Contains(t, result.Header.Get("Content-Security-Policy"), "img-src data:")
						}
						if method == http.MethodGet {
							assert.Equal(t, format.data, body)
						} else {
							assert.Empty(t, body)
						}
					}
				})
			}

			if s3Reads != nil {
				object, link := objects[0], links[0]
				expectedSize := object.ByteSize
				require.NoError(t, f.db.Model(&object).Update("byte_size", expectedSize+1).Error)
				mismatch := f.request(http.MethodGet, link, "", "", "", "")
				assert.Equal(t, http.StatusServiceUnavailable, mismatch.Code)
				assert.Empty(t, mismatch.Body.Bytes())
				require.NoError(t, f.db.Model(&object).Update("byte_size", expectedSize).Error)

				grant := f.grant(t)
				var canvasOwner model.CanvasGrant
				require.NoError(t, f.db.First(&canvasOwner, "id = ?", grant.GrantID).Error)
				canvasTask := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: task.UserId, TokenId: canvasOwner.TokenID, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: 1, ExpiresAt: task.ExpiresAt}
				require.NoError(t, f.db.Create(&canvasTask).Error)
				canvasObject := storedResultImage(t, profile, canvasTask, 0, formats[0].mime, formats[0].data)
				require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: canvasTask.TaskId, ObjectId: canvasObject.ObjectId}).Error)
				canvasPath := service.CanvasContentPath("images", canvasTask.TaskId, "0")

				s3GetMode.Store(storedResultS3Chunked)
				canvas := f.request(http.MethodGet, canvasPath, "", f.proof, "", grant.GrantID)
				assert.Equal(t, http.StatusOK, canvas.Code)
				assert.Equal(t, formats[0].data, canvas.Body.Bytes())
				public := f.request(http.MethodGet, link, "", "", "", "")
				assert.Equal(t, http.StatusOK, public.Code)
				assert.Equal(t, formats[0].data, public.Body.Bytes())
				assert.Equal(t, strconv.FormatInt(expectedSize, 10), public.Header().Get("Content-Length"))

				s3GetMode.Store(storedResultS3Truncated)
				canvas = f.request(http.MethodGet, canvasPath, "", f.proof, "", grant.GrantID)
				assert.Equal(t, http.StatusServiceUnavailable, canvas.Code)
				assert.Empty(t, canvas.Body.Bytes())
				s3GetMode.Store(0)

				cfg := service.DefaultImageRuntimeConfig()
				cfg.DownloadMaxBytes = 1
				require.NoError(t, service.SaveImageRuntimeConfig(t.Context(), cfg))
				before := s3Reads.Load()
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					assert.Equal(t, http.StatusRequestEntityTooLarge, f.request(method, link, "", "", "", "").Code)
				}
				assert.Equal(t, before, s3Reads.Load())
				return
			}
			object, link := objects[0], links[0]
			signed, err := service.ImageObjectURL(t.Context(), object, "", 3600)
			require.NoError(t, err)
			legacy := f.request(http.MethodGet, signed, "", "", "", "")
			require.Equal(t, http.StatusOK, legacy.Code, legacy.Body.String())
			assert.Equal(t, formats[0].data, legacy.Body.Bytes())
			for _, badLink := range []string{
				strings.TrimSuffix(link, ".png") + ".svg",
				link + ".png",
				storedResultTestPath + "..%2F" + strings.TrimPrefix(link, storedResultTestPath),
				storedResultTestPath + object.ObjectId + ".png",
			} {
				assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, badLink, "", "", "", "").Code, badLink)
			}
			for _, tc := range []struct {
				name, column string
				record       any
				disabled     any
				restored     any
			}{
				{"expired task", "expires_at", &task, time.Now().Unix() - 1, task.ExpiresAt},
				{"failed task", "status", &task, model.ImageTaskFailed, model.ImageTaskSucceeded},
				{"incomplete manifest", "result_count", &task, task.ResultCount + 1, task.ResultCount},
				{"expired object", "expires_at", &object, time.Now().Unix() - 1, object.ExpiresAt},
				{"inactive object", "status", &object, "deleted", "active"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					require.NoError(t, f.db.Model(tc.record).Update(tc.column, tc.disabled).Error)
					t.Cleanup(func() { require.NoError(t, f.db.Model(tc.record).Update(tc.column, tc.restored).Error) })
					assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, link, "", "", "", "").Code)
				})
			}
			inputTask := task
			inputTask.TaskId = "input_" + common.GetUUID()
			inputProfile := profile
			inputProfile.Class = "input"
			input := storedResultImage(t, inputProfile, inputTask, 0, formats[0].mime, formats[0].data)
			require.NoError(t, f.db.Create(&model.ImageInputObject{InputId: "sci_" + common.GetUUID(), TokenId: token.Id, ObjectId: input.ObjectId, KeyHash: common.GetUUID(), Status: "active", ExpiresAt: input.ExpiresAt}).Error)
			inputAlias := "img_" + common.GetUUID()
			require.NoError(t, f.db.Model(&input).Update("result_file_id", inputAlias).Error)
			assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, storedResultTestPath+inputAlias+".png", "", "", "", "").Code)

			grant := f.grant(t)
			var canvasOwner model.CanvasGrant
			require.NoError(t, f.db.First(&canvasOwner, "id = ?", grant.GrantID).Error)
			canvasTask := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: task.UserId, TokenId: canvasOwner.TokenID, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: 1, ExpiresAt: task.ExpiresAt}
			require.NoError(t, f.db.Create(&canvasTask).Error)
			require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: canvasTask.TaskId, ObjectId: object.ObjectId}).Error)
			assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, link, "", "", "", "").Code, "ordinary association cannot publish a Canvas result")
			assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, signed, "", "", "", "").Code)
		})
	}
}

func TestStoredResultFileVideoLifecycle(t *testing.T) {
	f, token := newStoredResultFixture(t)
	var hostingConnections atomic.Int32
	imageHost := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	imageHost.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		hostingConnections.Add(1)
		return nil, nil
	}}
	imageHost.StartTLS()
	t.Cleanup(imageHost.Close)
	hosting := service.DefaultImageHostingConfig()
	hosting.Enabled, hosting.AllowPrivateNetwork, hosting.BaseURL = true, true, imageHost.URL
	_, err := service.SaveImageHostingConfig(t.Context(), hosting, "video-isolation-upload-token", false, 0)
	require.NoError(t, err)
	f.router.GET("/v1/tasks/:key/artifacts", middleware.TokenAuth(), GetTaskArtifacts)
	gateway := httptest.NewServer(f.router)
	t.Cleanup(gateway.Close)
	require.NoError(t, f.db.AutoMigrate(&model.Task{}, &model.MediaInputObject{}, &model.ImageInputAlias{}))
	cfg := service.DefaultMediaRuntimeConfig()
	cfg.LocalPath, cfg.VideoAsyncEnabled = t.TempDir(), true
	require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
	now := time.Now().Unix()
	job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: f.user.Id, TokenId: token.Id, Status: "succeeded", StorageStatus: "succeeded", BillingStatus: "settled", CreatedAt: now, ExpiresAt: now + 3600}
	task := model.Task{TaskID: job.TaskId, UserId: job.UserId, Status: model.TaskStatusSuccess, PrivateData: model.TaskPrivateData{AsyncMedia: true, TokenId: token.Id}}
	require.NoError(t, f.db.Create(&job).Error)
	require.NoError(t, f.db.Create(&task).Error)
	video := taskMediaMP4Fixture()
	_, err = service.GetTaskArtifactStore().Persist(t.Context(), &task, types.TaskArtifact{Key: "video", Type: "video", MimeType: "video/mp4"}, bytes.NewReader(video))
	require.NoError(t, err)
	assert.Zero(t, hostingConnections.Load(), "enabled image hosting must not receive video uploads")
	var object model.MediaArtifactObject
	require.NoError(t, f.db.First(&object, "task_id = ?", job.TaskId).Error)
	query := "/v1/media/tasks_async/" + job.TaskId
	links := queryStoredResultFiles(t, f, query, token)
	require.Len(t, links, 1)
	link := links[0]
	require.Regexp(t, "^"+regexp.QuoteMeta(storedResultTestPath)+"vid_[0-9a-f]{32}\\.mp4$", link)
	assert.NotContains(t, link, object.ObjectId)
	manifest := f.request(http.MethodGet, query, "", "Bearer "+token.Key, "", "")
	require.Equal(t, http.StatusOK, manifest.Code, manifest.Body.String())
	assert.Contains(t, manifest.Body.String(), `"url":"`+link+`"`)
	assert.Contains(t, manifest.Body.String(), `"view_url":"`+link+`"`)
	artifacts := f.request(http.MethodGet, "/v1/tasks/"+task.TaskID+"/artifacts", "", "Bearer "+token.Key, "", "")
	require.Equal(t, http.StatusOK, artifacts.Code, artifacts.Body.String())
	assert.Contains(t, artifacts.Body.String(), `"content_url":"`+link+`"`)

	for _, tc := range []struct {
		method, rangeHeader string
		status              int
		body                []byte
	}{
		{http.MethodGet, "", http.StatusOK, video},
		{http.MethodHead, "", http.StatusOK, nil},
		{http.MethodGet, "bytes=0-7", http.StatusPartialContent, video[:8]},
		{http.MethodHead, "bytes=0-7", http.StatusPartialContent, nil},
	} {
		request, err := http.NewRequest(tc.method, gateway.URL+strings.TrimPrefix(link, storedResultTestBase), nil)
		require.NoError(t, err)
		request.Header.Set("Range", tc.rangeHeader)
		result, err := gateway.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(result.Body)
		require.NoError(t, result.Body.Close())
		require.NoError(t, err)
		require.Equal(t, tc.status, result.StatusCode, string(body))
		assert.Equal(t, "video/mp4", result.Header.Get("Content-Type"))
		assert.Empty(t, result.Header.Get("Location"))
		if tc.status == http.StatusPartialContent {
			assert.Equal(t, "bytes 0-7/"+strconv.Itoa(len(video)), result.Header.Get("Content-Range"))
		}
		assert.True(t, bytes.Equal(tc.body, body))
	}
	legacyLink, err := service.MediaObjectURL(object, "", 3600)
	require.NoError(t, err)
	legacy := f.request(http.MethodGet, legacyLink, "", "", "", "")
	require.Equal(t, http.StatusOK, legacy.Code, legacy.Body.String())
	assert.Equal(t, video, legacy.Body.Bytes())
	assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, strings.TrimSuffix(link, ".mp4")+".svg", "", "", "", "").Code)
	assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, storedResultTestPath+object.ObjectId+".mp4", "", "", "", "").Code)

	t.Run("reference reuse", func(t *testing.T) {
		profile := model.ImageStorageProfile{ProfileId: "video-reference-image", Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}
		require.NoError(t, f.db.Create(&profile).Error)
		imageTask := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: job.UserId, TokenId: job.TokenId, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: 1, CreatedAt: now, ExpiresAt: job.ExpiresAt}
		require.NoError(t, f.db.Create(&imageTask).Error)
		var pngData bytes.Buffer
		require.NoError(t, png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))))
		imageObject := storedResultImage(t, profile, imageTask, 0, "image/png", pngData.Bytes())
		require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: imageTask.TaskId, ObjectId: imageObject.ObjectId}).Error)
		imageLinks := queryStoredResultFiles(t, f, "/v1/images/tasks_async/"+imageTask.TaskId, token)
		require.Len(t, imageLinks, 1)
		imageLink := imageLinks[0]
		input := service.AsyncVideoRequest{
			Body:                     []byte(`{"references":[{"type":"image","role":"reference","source":"` + imageLink + `"},{"type":"video","role":"reference","source":"` + link + `"}]}`),
			ReferenceRecoveryVersion: 1, ReferenceRecoveryDeadline: now + 1200, ReferenceURLExpiresAt: now + 1800,
			ReferenceInputs: []service.AsyncVideoReferenceInput{
				{Index: 0, Kind: "image", Role: "reference", Path: "/references/0/source", Source: imageLink},
				{Index: 1, Kind: "video", Role: "reference", Path: "/references/1/source", Source: link},
			},
		}
		newJob := job
		newJob.TaskId, newJob.ExpiresAt = model.GenerateTaskID(), now+7200
		prepared, err := service.PrepareAsyncVideoLocalReferences(t.Context(), newJob, input)
		require.NoError(t, err)
		assert.True(t, prepared.LocalReferencesReady)
		assert.JSONEq(t, string(input.Body), string(prepared.Body))
		assert.Empty(t, prepared.ReferenceInputs[0].ObjectId)
		assert.Empty(t, prepared.ReferenceInputs[1].ObjectId)
		for _, other := range []model.AsyncMediaJob{
			{TaskId: newJob.TaskId, UserId: job.UserId, TokenId: token.Id + 1, CreatedAt: now, ExpiresAt: newJob.ExpiresAt},
			{TaskId: newJob.TaskId, UserId: job.UserId + 1, TokenId: token.Id, CreatedAt: now, ExpiresAt: newJob.ExpiresAt},
		} {
			_, err := service.PrepareAsyncVideoLocalReferences(t.Context(), other, input)
			assert.ErrorIs(t, err, service.ErrMediaInputUnavailable)
		}
		input.ReferenceURLExpiresAt = job.ExpiresAt + 1
		_, err = service.PrepareAsyncVideoLocalReferences(t.Context(), newJob, input)
		assert.ErrorIs(t, err, service.ErrMediaInputUnavailable)
	})

	require.NoError(t, f.db.Model(&job).Update("storage_status", "upstream").Error)
	assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, link, "", "", "", "").Code)
	require.NoError(t, f.db.Model(&job).Update("storage_status", "succeeded").Error)
	require.NoError(t, f.db.Model(&token).Update("status", common.TokenStatusDisabled).Error)
	assert.Equal(t, http.StatusNotFound, f.request(http.MethodGet, link, "", "", "", "").Code)
}

func TestStoredResultFileMigrationAndStableIssuance(t *testing.T) {
	f, token := newStoredResultFixture(t)
	versionSQL := "SELECT sqlite_version()"
	if f.db.Dialector.Name() != "sqlite" {
		versionSQL = "SELECT version()"
	}
	var version string
	require.NoError(t, f.db.Raw(versionSQL).Scan(&version).Error)
	t.Logf("result file migration: %s %s", f.db.Dialector.Name(), version)
	for _, schema := range []any{&model.ImageStorageObject{}, &model.MediaArtifactObject{}} {
		// The prior schema differs only by this nullable alias and its index.
		require.NoError(t, f.db.Migrator().DropIndex(schema, "ResultFileId"))
		require.NoError(t, f.db.Migrator().DropColumn(schema, "ResultFileId"))
	}
	now := time.Now().Unix()
	images := make([]model.ImageStorageObject, 2)
	videos := make([]model.MediaArtifactObject, 2)
	for i := range images {
		task := model.AsyncImageTask{TaskId: "asyncimg_" + common.GetUUID(), UserId: f.user.Id, TokenId: token.Id, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: 1, CreatedAt: now, ExpiresAt: now + 3600}
		require.NoError(t, f.db.Create(&task).Error)
		images[i] = model.ImageStorageObject{ObjectId: "imo_" + common.GetUUID(), IdentityHash: common.GetUUID(), ProfileId: "historical-profile", Class: "temporary", ObjectKey: "historical-" + strconv.Itoa(i) + ".png", ContentType: "image/png", Status: "active", ByteSize: 123, Checksum: "preserved-image-checksum", CreatedAt: now, ExpiresAt: task.ExpiresAt}
		require.NoError(t, f.db.Omit("ResultFileId").Create(&images[i]).Error)
		require.NoError(t, f.db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ObjectId: images[i].ObjectId}).Error)
		job := model.AsyncMediaJob{TaskId: model.GenerateTaskID(), IdentityHash: common.GetUUID(), UserId: f.user.Id, TokenId: token.Id, Status: "succeeded", StorageStatus: "succeeded", BillingStatus: "settled", ExpiresAt: now + 3600}
		require.NoError(t, f.db.Create(&job).Error)
		videos[i] = model.MediaArtifactObject{ObjectId: "media_" + common.GetUUID(), IdentityHash: common.GetUUID(), TaskId: job.TaskId, ArtifactKey: "video", UserId: f.user.Id, TokenId: token.Id, RootPath: "historical-root", ObjectKey: "historical-" + strconv.Itoa(i) + ".mp4", MimeType: "video/mp4", Status: "active", ByteSize: 456, Checksum: "preserved-video-checksum", CreatedAt: now, ExpiresAt: job.ExpiresAt}
		require.NoError(t, f.db.Omit("ResultFileId").Create(&videos[i]).Error)
	}
	for range 2 {
		require.NoError(t, f.db.AutoMigrate(&model.ImageStorageObject{}, &model.MediaArtifactObject{}))
	}
	for i := range images {
		var imageObject model.ImageStorageObject
		var videoObject model.MediaArtifactObject
		require.NoError(t, f.db.First(&imageObject, images[i].Id).Error)
		require.NoError(t, f.db.First(&videoObject, videos[i].ID).Error)
		assert.Equal(t, images[i], imageObject)
		assert.Equal(t, videos[i], videoObject)
	}
	for _, schema := range []any{&model.ImageStorageObject{}, &model.MediaArtifactObject{}} {
		assert.True(t, f.db.Migrator().HasIndex(schema, "ObjectId"))
		assert.True(t, f.db.Migrator().HasIndex(schema, "IdentityHash"))
		assert.True(t, f.db.Migrator().HasIndex(schema, "ResultFileId"))
	}
	for _, kind := range []string{"image", "video"} {
		t.Run(kind+" concurrent first query", func(t *testing.T) {
			var wg sync.WaitGroup
			links := make([]string, 2)
			errors := make([]error, 2)
			for i := range links {
				wg.Go(func() {
					// Both callers hold the same pre-issuance object snapshot.
					if kind == "image" {
						links[i], errors[i] = service.ImageResultFileURL(t.Context(), images[0], storedResultTestBase)
					} else {
						links[i], errors[i] = service.MediaResultFileURL(t.Context(), videos[0], storedResultTestBase)
					}
				})
			}
			wg.Wait()
			for _, err := range errors {
				require.NoError(t, err)
			}
			assert.Equal(t, links[0], links[1])
			assert.NotContains(t, links[0], "?")
			assert.NotContains(t, links[0], images[0].ObjectId)
			assert.NotContains(t, links[0], videos[0].ObjectId)
			require.NoError(t, f.db.AutoMigrate(&model.ImageStorageObject{}, &model.MediaArtifactObject{}))
			if kind == "image" {
				var current model.ImageStorageObject
				require.NoError(t, f.db.First(&current, images[0].Id).Error)
				link, err := service.ImageResultFileURL(t.Context(), current, storedResultTestBase)
				require.NoError(t, err)
				assert.Equal(t, links[0], link)
				assert.Error(t, f.db.Model(&images[1]).UpdateColumn("result_file_id", current.ResultFileId).Error)
				assert.Error(t, f.db.Model(&images[1]).UpdateColumn("object_id", images[0].ObjectId).Error)
				assert.Error(t, f.db.Model(&images[1]).UpdateColumn("identity_hash", images[0].IdentityHash).Error)
			} else {
				var current model.MediaArtifactObject
				require.NoError(t, f.db.First(&current, videos[0].ID).Error)
				link, err := service.MediaResultFileURL(t.Context(), current, storedResultTestBase)
				require.NoError(t, err)
				assert.Equal(t, links[0], link)
				assert.Error(t, f.db.Model(&videos[1]).UpdateColumn("result_file_id", current.ResultFileId).Error)
				assert.Error(t, f.db.Model(&videos[1]).UpdateColumn("object_id", videos[0].ObjectId).Error)
				assert.Error(t, f.db.Model(&videos[1]).UpdateColumn("identity_hash", videos[0].IdentityHash).Error)
			}
		})
	}
}
