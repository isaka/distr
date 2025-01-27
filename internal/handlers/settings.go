package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/httprate"

	"github.com/getsentry/sentry-go"
	"github.com/glasskube/cloud/api"
	"github.com/glasskube/cloud/internal/apierrors"
	"github.com/glasskube/cloud/internal/auth"
	"github.com/glasskube/cloud/internal/authkey"
	internalctx "github.com/glasskube/cloud/internal/context"
	"github.com/glasskube/cloud/internal/db"
	"github.com/glasskube/cloud/internal/mailsending"
	"github.com/glasskube/cloud/internal/mapping"
	"github.com/glasskube/cloud/internal/middleware"
	"github.com/glasskube/cloud/internal/security"
	"github.com/glasskube/cloud/internal/types"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

func SettingsRouter(r chi.Router) {
	r.Post("/user", userSettingsUpdateHandler)
	r.Route("/verify", func(r chi.Router) {
		r.With(requestVerificationMailRateLimitPerUser).Post("/request", userSettingsVerifyRequestHandler)
		r.Post("/confirm", userSettingsVerifyConfirmHandler)
	})
	r.Route("/tokens", func(r chi.Router) {
		r.Get("/", getAccessTokensHandler())
		r.Post("/", createAccessTokenHandler())
		r.Route("/{id}", func(r chi.Router) {
			r.Delete("/", deleteAccessTokenHandler())
		})
	})
}

func userSettingsUpdateHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := internalctx.GetLogger(ctx)
	auth := auth.Authentication.Require(ctx)
	body, err := JsonBody[api.UpdateUserAccountRequest](w, r)
	if err != nil {
		return
	}

	if err := body.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	user, err := db.GetUserAccountByID(ctx, auth.CurrentUserID())
	if err != nil {
		log.Error("failed to get current user", zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.Name != "" {
		user.Name = body.Name
	}
	if body.Password != nil {
		user.Password = *body.Password
		if err := security.HashPassword(user); err != nil {
			sentry.GetHubFromContext(ctx).CaptureException(err)
			log.Error("failed to hash password", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}

	if err := db.UpdateUserAccount(ctx, user); errors.Is(err, apierrors.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusBadRequest)
	} else if err != nil {
		log.Error("failed to update user", zap.Error(err))
		sentry.GetHubFromContext(ctx).CaptureException(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	} else {
		RespondJSON(w, user)
	}
}

func userSettingsVerifyRequestHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	if userAccount, err := db.GetUserAccountByID(ctx, auth.CurrentUserID()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if userAccount.EmailVerifiedAt != nil {
		w.WriteHeader(http.StatusNoContent)
	} else if err := mailsending.SendUserVerificationMail(ctx, *userAccount); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		sentry.GetHubFromContext(ctx).CaptureException(err)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func userSettingsVerifyConfirmHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := internalctx.GetLogger(ctx)
	auth := auth.Authentication.Require(ctx)
	if userAccount, err := db.GetUserAccountByID(ctx, auth.CurrentUserID()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	} else if !auth.CurrentUserEmailVerified() {
		http.Error(w, "token does not have verified claim", http.StatusForbidden)
	} else if err := db.UpdateUserAccountEmailVerified(ctx, userAccount); err != nil {
		if errors.Is(err, apierrors.ErrNotFound) {
			http.Error(w, "could not update user", http.StatusBadRequest)
		} else {
			log.Error("could not update user", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, "could not update user", http.StatusInternalServerError)
		}
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func getAccessTokensHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		auth := auth.Authentication.Require(ctx)
		tokens, err := db.GetAccessTokensByUserAccountID(ctx, auth.CurrentUserID())
		if err != nil {
			log.Warn("error getting tokens", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			RespondJSON(w, mapping.List(tokens, mapping.AccessTokenToDTO))
		}
	}
}

func createAccessTokenHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		auth := auth.Authentication.Require(ctx)
		request, err := JsonBody[api.CreateAccessTokenRequest](w, r)
		if err != nil {
			return
		}

		key, err := authkey.NewKey()
		if err != nil {
			log.Warn("error creating token", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		token := types.AccessToken{
			ExpiresAt:     request.ExpiresAt,
			Label:         request.Label,
			UserAccountID: auth.CurrentUserID(),
			Key:           key,
		}
		if err := db.CreateAccessToken(ctx, &token); err != nil {
			log.Warn("error creating token", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			RespondJSON(w, mapping.AccessTokenToDTO(token).WithKey(token.Key))
		}
	}
}

func deleteAccessTokenHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		tokenID := r.PathValue("id")
		auth := auth.Authentication.Require(ctx)
		if err := db.DeleteAccessToken(ctx, tokenID, auth.CurrentUserID()); err != nil {
			log.Warn("error deleting token", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

var requestVerificationMailRateLimitPerUser = httprate.Limit(
	3,
	10*time.Minute,
	httprate.WithKeyFuncs(middleware.RateLimitCurrentUserIdKeyFunc),
)
