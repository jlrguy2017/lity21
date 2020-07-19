package sgtm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/gosimple/slug"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
	"moul.io/sgtm/pkg/sgtmpb"
)

const (
	oauthTokenCookie = "oauth-token"
	// sessionError
)

func (svc *Service) parseJWTToken(tokenString string) (*jwtClaims, error) {
	keyFunc := func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(svc.opts.JWTSigningKey), nil
	}
	token, err := jwt.ParseWithClaims(tokenString, &jwtClaims{}, keyFunc)
	if err != nil {
		return nil, fmt.Errorf("parse with claims: %w", err)
	}

	claims, ok := token.Claims.(*jwtClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid token")
	}

	if claims.Audience != "sgtm" {
		return nil, errors.New("invalid audience")
	}

	return claims, nil
}

func (svc *Service) httpAuthLogin(w http.ResponseWriter, r *http.Request) {
	conf := svc.authConfigFromReq(r)
	state := svc.authGenerateState(r)
	url := conf.AuthCodeURL(state)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (svc *Service) httpAuthLogout(w http.ResponseWriter, r *http.Request) {
	cookie := http.Cookie{
		Name:     oauthTokenCookie,
		Value:    "",
		HttpOnly: true,
		MaxAge:   -1,
		Path:     "/",
	}
	http.SetCookie(w, &cookie)
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func (svc *Service) httpAuthCallback(w http.ResponseWriter, r *http.Request) {
	conf := svc.authConfigFromReq(r)

	// verifiy oauth2 state
	{
		// FIXME: store state in cookie instead?
		got := r.URL.Query().Get("state")
		expected := svc.authGenerateState(r)
		if expected != got {
			svc.errRenderHTML(w, r, fmt.Errorf("invalid oauth2 state"), http.StatusBadRequest)
			return
		}
	}

	// exchange the code
	var token *oauth2.Token
	{
		code := r.URL.Query().Get("code")
		var err error
		token, err = conf.Exchange(context.Background(), code)
		if err != nil {
			svc.errRenderHTML(w, r, err, http.StatusUnprocessableEntity)
			return
		}
	}

	// get user's info
	var discordUser discordgo.User
	{
		res, err := conf.Client(context.Background(), token).Get("https://discordapp.com/api/v6/users/@me")
		if err != nil {
			svc.errRenderHTML(w, r, err, http.StatusUnprocessableEntity)
			return
		}
		defer res.Body.Close()
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			svc.errRenderHTML(w, r, err, http.StatusUnprocessableEntity)
			return
		}
		if err := json.Unmarshal(body, &discordUser); err != nil {
			svc.errRenderHTML(w, r, err, http.StatusUnprocessableEntity)
			return
		}
		if !discordUser.Verified {
			svc.errRenderHTML(w, r, fmt.Errorf("email not verified"), http.StatusForbidden)
			return
		}
		if discordUser.Bot {
			svc.errRenderHTML(w, r, fmt.Errorf("access denied for bots"), http.StatusForbidden)
			return
		}
		svc.logger.Debug("get user settings", zap.Any("user", discordUser))
	}

	// create/update user in DB
	var dbUser sgtmpb.User
	{
		dbUser.Email = discordUser.Email
		err := svc.db.Where(&dbUser).First(&dbUser).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			// user not found, creating it
			dbUser = sgtmpb.User{
				Email:           discordUser.Email,
				Avatar:          fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png", discordUser.ID, discordUser.Avatar),
				Slug:            slug.Make(discordUser.Username),
				Locale:          discordUser.Locale,
				DiscordID:       discordUser.ID,
				DiscordUsername: fmt.Sprintf("%s#%s", discordUser.Username, discordUser.Discriminator),
				// Firstname
				// Lastname
			}
			// FIXME: check if slug already exists, if yes, append something to the slug
			err = svc.db.Transaction(func(tx *gorm.DB) error {
				if err := svc.db.Create(&dbUser).Error; err != nil {
					return err
				}

				registerEvent := sgtmpb.Post{AuthorID: dbUser.ID, Kind: sgtmpb.Post_RegisterKind}
				if err := svc.db.Create(&registerEvent).Error; err != nil {
					return err
				}
				svc.logger.Debug("new register", zap.Any("event", &registerEvent))

				linkDiscordEvent := sgtmpb.Post{AuthorID: dbUser.ID, Kind: sgtmpb.Post_LinkDiscordAccountKind}
				if err := svc.db.Create(&linkDiscordEvent).Error; err != nil {
					return err
				}
				svc.logger.Debug("new link discord event", zap.Any("event", &registerEvent))

				return nil
			})
			if err != nil {
				svc.errRenderHTML(w, r, err, http.StatusUnprocessableEntity)
				return
			}

		case err == nil:
			// user exists
			// FIXME: update user in DB if needed

			loginEvent := sgtmpb.Post{AuthorID: dbUser.ID, Kind: sgtmpb.Post_LoginKind}
			if err := svc.db.Create(&loginEvent).Error; err != nil {
				svc.errRenderHTML(w, r, err, http.StatusUnprocessableEntity)
				return
			}
			svc.logger.Debug("new login", zap.Any("event", &loginEvent))

		default:
			// unexpected error
			svc.errRenderHTML(w, r, err, http.StatusUnprocessableEntity)
			return
		}
	}

	// prepare JWT token
	var tokenString string
	{
		session := &sgtmpb.Session{
			UserID:             dbUser.ID,
			DiscordAccessToken: token.AccessToken,
		}
		svc.logger.Debug("user session", zap.Any("session", session))
		sessionID := fmt.Sprintf("%d", svc.opts.Snowflake.Generate().Int64())
		claims := jwtClaims{
			Session: session,
			StandardClaims: jwt.StandardClaims{
				Id:        sessionID,
				ExpiresAt: token.Expiry.Unix(),
				Issuer:    "discord",
				IssuedAt:  time.Now().Unix(),
				Audience:  "sgtm",
				// Subject: username/email,
			},
		}
		jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		var err error
		tokenString, err = jwtToken.SignedString([]byte(svc.opts.JWTSigningKey))
		if err != nil {
			svc.errRenderHTML(w, r, err, http.StatusUnprocessableEntity)
			return
		}
		svc.logger.Debug("token string", zap.String("token", tokenString))
	}

	// set user cookie and redirect to homepage
	{
		cookie := http.Cookie{
			Name:     oauthTokenCookie,
			Value:    tokenString,
			Expires:  token.Expiry,
			HttpOnly: true,
			Path:     "/",
			//Domain:   r.Host,
		}
		svc.logger.Debug("set user cookie", zap.Any("cookie", cookie))
		http.SetCookie(w, &cookie)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
	}
}

func (svc *Service) authConfigFromReq(r *http.Request) *oauth2.Config {
	hostname := svc.opts.Hostname
	if hostname == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		hostname = fmt.Sprintf("%s://%s", scheme, r.Host)
	}
	return &oauth2.Config{
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://discordapp.com/api/oauth2/authorize",
			TokenURL:  "https://discordapp.com/api/oauth2/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		Scopes:       []string{"identify", "email"},
		RedirectURL:  hostname + "/auth/callback",
		ClientID:     svc.opts.DiscordClientID,
		ClientSecret: svc.opts.DiscordClientSecret,
	}
}

func (svc *Service) authGenerateState(r *http.Request) string {
	// FIXME: add IP too?
	csum := sha256.Sum256([]byte(r.UserAgent() + svc.opts.DiscordClientSecret))
	return base64.StdEncoding.EncodeToString(csum[:])
}

type jwtClaims struct {
	Session            *sgtmpb.Session `json:"session"`
	jwt.StandardClaims `json:"standard"`
}
