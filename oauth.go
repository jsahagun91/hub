package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/sirupsen/logrus"
	"github.com/skip2/go-qrcode"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

type TemplateRegistry struct {
	templates map[string]*template.Template
}

type AlbyOAuthService struct {
	cfg       *Config
	oauthConf *oauth2.Config
	db        *gorm.DB
	e         *echo.Echo
}

//go:embed public/*
var embeddedAssets embed.FS

//go:embed views/*
var embeddedViews embed.FS

// Implement e.Renderer interface
func (t *TemplateRegistry) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	tmpl, ok := t.templates[name]
	if !ok {
		err := errors.New("Template not found -> " + name)
		return err
	}
	return tmpl.ExecuteTemplate(w, "layout.html", data)
}

func (svc *AlbyOAuthService) Start(ctx context.Context) (err error) {
	// Start server
	go func() {
		if err := svc.e.Start(fmt.Sprintf(":%v", svc.cfg.OAuthServerPort)); err != nil && err != http.ErrServerClosed {
			svc.e.Logger.Fatal("shutting down the server")
		}
	}()
	<-ctx.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return svc.e.Shutdown(ctx)
}

func NewAlbyOauthService(svc *Service) (result *AlbyOAuthService, err error) {
	conf := &oauth2.Config{
		ClientID:     svc.cfg.AlbyClientId,
		ClientSecret: svc.cfg.AlbyClientSecret,
		//Todo: do we really need all these permissions?
		Scopes: []string{"account:read", "payments:send", "invoices:read", "transactions:read", "invoices:create"},
		Endpoint: oauth2.Endpoint{
			TokenURL: svc.cfg.OAuthTokenUrl,
			AuthURL:  svc.cfg.OAuthAuthUrl,
		},
		RedirectURL: svc.cfg.OAuthRedirectUrl,
	}

	albySvc := &AlbyOAuthService{
		cfg:       svc.cfg,
		oauthConf: conf,
		db:        svc.db,
	}

	e := echo.New()
	templates := make(map[string]*template.Template)
	templates["apps/index.html"] = template.Must(template.ParseFS(embeddedViews, "views/apps/index.html", "views/layout.html"))
	templates["apps/new.html"] = template.Must(template.ParseFS(embeddedViews, "views/apps/new.html", "views/layout.html"))
	templates["apps/show.html"] = template.Must(template.ParseFS(embeddedViews, "views/apps/show.html", "views/layout.html"))
	templates["index.html"] = template.Must(template.ParseFS(embeddedViews, "views/index.html", "views/layout.html"))
	e.Renderer = &TemplateRegistry{
		templates: templates,
	}
	e.HideBanner = true
	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(middleware.Logger())
	e.Use(session.Middleware(sessions.NewCookieStore([]byte("secret"))))
	assetSubdir, err := fs.Sub(embeddedAssets, "public")
	assetHandler := http.FileServer(http.FS(assetSubdir))
	e.GET("/public/*", echo.WrapHandler(http.StripPrefix("/public/", assetHandler)))
	e.GET("/", albySvc.IndexHandler)
	e.GET("/alby/auth", albySvc.AuthHandler)
	e.GET("/alby/callback", albySvc.CallbackHandler)
	e.GET("/apps", albySvc.AppsListHandler)
	e.GET("/apps/new", albySvc.AppsNewHandler)
	e.GET("/qr", albySvc.QRHandler)
	e.GET("/apps/:id", albySvc.AppsShowHandler)
	e.POST("/apps", albySvc.AppsCreateHandler)
	e.POST("/apps/delete/:id", albySvc.AppsDeleteHandler)
	e.GET("/logout", albySvc.LogoutHandler)
	albySvc.e = e
	return albySvc, err
}

func (svc *AlbyOAuthService) SendPaymentSync(ctx context.Context, senderPubkey, payReq string) (preimage string, err error) {
	logrus.Infof("Processing payment request %s from %s", payReq, senderPubkey)
	app := App{}
	err = svc.db.Preload("User").First(&app, &App{
		NostrPubkey: senderPubkey,
	}).Error
	if err != nil {
		return "", err
	}
	user := app.User
	client := svc.oauthConf.Client(ctx, &oauth2.Token{
		AccessToken:  user.AccessToken,
		RefreshToken: user.RefreshToken,
		Expiry:       user.Expiry,
	})
	body := bytes.NewBuffer([]byte{})
	payload := &PayRequest{
		Invoice: payReq,
	}
	err = json.NewEncoder(body).Encode(payload)
	resp, err := client.Post(fmt.Sprintf("%s/payments/bolt11", svc.cfg.AlbyAPIURL), "application/json", body)
	if err != nil {
		return "", err
	}
	//todo non-200 status code handling
	responsePayload := &PayResponse{}
	err = json.NewDecoder(resp.Body).Decode(responsePayload)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 300 {
		logrus.Infof("Sent payment with hash %s preimage %s", responsePayload.PaymentHash, responsePayload.Preimage)
		return responsePayload.Preimage, nil
	} else {
		return "", errors.New("Failed")
	}
}

func (svc *AlbyOAuthService) IndexHandler(c echo.Context) error {
	return c.Render(http.StatusOK, "index.html", map[string]interface{}{})
}

func (svc *AlbyOAuthService) LogoutHandler(c echo.Context) error {
	sess, _ := session.Get("alby_nostr_wallet_connect", c)
	sess.Values["user_id"] = ""
	delete(sess.Values, "user_id")
	sess.Options = &sessions.Options{
		MaxAge: -1,
	}
	sess.Save(c.Request(), c.Response())
	return c.Redirect(http.StatusMovedPermanently, "/")
}

func (svc *AlbyOAuthService) AppsListHandler(c echo.Context) error {
	sess, _ := session.Get("alby_nostr_wallet_connect", c)
	userID := sess.Values["user_id"]
	if userID == nil {
		return c.Redirect(http.StatusMovedPermanently, "/alby/auth")
	}

	user := User{}
	svc.db.Preload("Apps").First(&user, userID)
	apps := user.Apps
	return c.Render(http.StatusOK, "apps/index.html", map[string]interface{}{
		"NostrWalletConnect": fmt.Sprintf("%s?relay=%s", svc.cfg.IdentityPubkey, url.QueryEscape(svc.cfg.Relay)),
		"Apps":               apps,
		"User":               user,
	})
}

func (svc *AlbyOAuthService) AppsShowHandler(c echo.Context) error {
	sess, _ := session.Get("alby_nostr_wallet_connect", c)
	userID := sess.Values["user_id"]
	if userID == nil {
		return c.Redirect(http.StatusMovedPermanently, "/alby/auth")
	}

	user := User{}
	svc.db.Preload("Apps").First(&user, userID)
	app := App{}
	svc.db.Where("user_id = ?", user.ID).First(&app, c.Param("id"))
	return c.Render(http.StatusOK, "apps/show.html", map[string]interface{}{
		"App":  app,
		"User": user,
	})
}

func (svc *AlbyOAuthService) AppsNewHandler(c echo.Context) error {
	sess, _ := session.Get("alby_nostr_wallet_connect", c)
	userID := sess.Values["user_id"]
	if userID == nil {
		return c.Redirect(http.StatusMovedPermanently, "/alby/auth")
	}
	user := User{}
	svc.db.First(&user, userID)

	return c.Render(http.StatusOK, "apps/new.html", map[string]interface{}{
		"User": user,
	})
}

func (svc *AlbyOAuthService) AppsCreateHandler(c echo.Context) error {
	sess, _ := session.Get("alby_nostr_wallet_connect", c)
	userID := sess.Values["user_id"]
	if userID == nil {
		return c.Redirect(http.StatusMovedPermanently, "/alby/auth")
	}
	user := User{}
	svc.db.Preload("Apps").First(&user, userID)

	svc.db.Model(&user).Association("Apps").Append(&App{Name: c.FormValue("name"), NostrPubkey: c.FormValue("pubkey")})
	return c.Redirect(http.StatusMovedPermanently, "/apps")
}

func (svc *AlbyOAuthService) AppsDeleteHandler(c echo.Context) error {
	sess, _ := session.Get("alby_nostr_wallet_connect", c)
	userID := sess.Values["user_id"]
	if userID == nil {
		return c.Redirect(http.StatusMovedPermanently, "/alby/auth")
	}
	user := User{}
	svc.db.Preload("Apps").First(&user, userID)
	app := App{}
	svc.db.Where("user_id = ?", user.ID).First(&app, c.Param("id"))
	svc.db.Delete(&app)
	return c.Redirect(http.StatusMovedPermanently, "/apps")
}

func (svc *AlbyOAuthService) AuthHandler(c echo.Context) error {
	url := svc.oauthConf.AuthCodeURL("")
	return c.Redirect(http.StatusMovedPermanently, url)
}

func (svc *AlbyOAuthService) QRHandler(c echo.Context) error {
	img, err := qrcode.Encode(fmt.Sprintf("nostrwalletconnect://%s?relay=%s", svc.cfg.IdentityPubkey, svc.cfg.Relay), qrcode.High, 256)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "img/png", img)
}

func (svc *AlbyOAuthService) CallbackHandler(c echo.Context) error {
	code := c.QueryParam("code")
	tok, err := svc.oauthConf.Exchange(c.Request().Context(), code)
	if err != nil {
		svc.e.Logger.Error(err)
		return err
	}
	client := svc.oauthConf.Client(c.Request().Context(), tok)
	res, err := client.Get(fmt.Sprintf("%s/user/me", svc.cfg.AlbyAPIURL))
	if err != nil {
		svc.e.Logger.Error(err)
		return err
	}
	me := AlbyMe{}
	err = json.NewDecoder(res.Body).Decode(&me)
	if err != nil {
		svc.e.Logger.Error(err)
		return err
	}
	_, pubkey, err := nip19.Decode(me.NPub)
	if err != nil {
		svc.e.Logger.Error(err)
		return err
	}

	user := User{}
	svc.db.FirstOrInit(&user, User{AlbyIdentifier: me.Identifier})
	user.AccessToken = tok.AccessToken
	user.RefreshToken = tok.RefreshToken
	user.Expiry = tok.Expiry // TODO; probably needs some calculation
	svc.db.Save(&user)

	app := App{}
	svc.db.FirstOrInit(&app, App{UserId: user.ID, NostrPubkey: pubkey.(string)})
	app.Name = me.LightningAddress
	app.Description = "All apps with your private key"
	svc.db.Save(&app)

	sess, _ := session.Get("alby_nostr_wallet_connect", c)
	sess.Options = &sessions.Options{
		Path:   "/",
		MaxAge: 0, // TODO: how to session cookie?
	}
	sess.Values["user_id"] = user.ID
	sess.Save(c.Request(), c.Response())
	return c.Redirect(http.StatusMovedPermanently, "/apps")
}