package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getsentry/sentry-go"
	"github.com/glasskube/distr/api"
	"github.com/glasskube/distr/internal/agentclient/useragent"
	"github.com/glasskube/distr/internal/agentmanifest"
	"github.com/glasskube/distr/internal/apierrors"
	"github.com/glasskube/distr/internal/auth"
	"github.com/glasskube/distr/internal/authjwt"
	internalctx "github.com/glasskube/distr/internal/context"
	"github.com/glasskube/distr/internal/db"
	"github.com/glasskube/distr/internal/env"
	"github.com/glasskube/distr/internal/middleware"
	"github.com/glasskube/distr/internal/security"
	"github.com/glasskube/distr/internal/types"
	"github.com/glasskube/distr/internal/util"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func AgentRouter(r chi.Router) {
	r.With(
		queryAuthDeploymentTargetCtxMiddleware,
	).Group(func(r chi.Router) {
		r.Get("/connect", connectHandler())
	})
	r.Route("/agent", func(r chi.Router) {
		// agent login (from basic auth to token)
		r.Post("/login", agentLoginHandler)

		r.With(
			auth.AgentAuthentication.Middleware,
			middleware.AgentSentryUser,
			agentAuthDeploymentTargetCtxMiddleware,
			rateLimitPerAgent,
		).Group(func(r chi.Router) {
			// agent routes, authenticated via token
			r.Get("/manifest", agentManifestHandler())
			r.Get("/resources", agentResourcesHandler)
			r.Post("/status", angentPostStatusHandler)
		})
	})
}

func connectHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		deploymentTarget := internalctx.GetDeploymentTarget(ctx)

		if deploymentTarget.CurrentStatus != nil &&
			deploymentTarget.CurrentStatus.CreatedAt.Add(2*env.AgentInterval()).After(time.Now()) {
			http.Error(
				w,
				fmt.Sprintf(
					"deployment target is already connected and appears to be still running (last status %v)",
					deploymentTarget.CurrentStatus.CreatedAt),
				http.StatusBadRequest,
			)
			return
		}

		secret := r.URL.Query().Get("targetSecret")
		if manifest, err := agentmanifest.Get(ctx, *deploymentTarget, &secret); err != nil {
			log.Error("could not get agent manifest", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.Header().Add("Content-Type", "application/yaml")
			if _, err := io.Copy(w, manifest); err != nil {
				log.Warn("writing to client failed", zap.Error(err))
			}
		}
	}
}

func agentLoginHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := internalctx.GetLogger(ctx)

	if targetId, targetSecret, ok := r.BasicAuth(); !ok {
		log.Error("invalid Basic Auth")
		w.WriteHeader(http.StatusUnauthorized)
	} else if parsedTargetId, err := uuid.Parse(targetId); err != nil {
		http.Error(w, "targetId is not a valid UUID", http.StatusBadRequest)
	} else if agentLoginPerTargetIdRateLimiter.RespondOnLimit(w, r, targetId) {
		return
	} else if deploymentTarget, err := getVerifiedDeploymentTarget(ctx, parsedTargetId, targetSecret); err != nil {
		log.Error("failed to get deployment target from query auth", zap.Error(err))
		w.WriteHeader(http.StatusUnauthorized)
	} else {
		// TODO maybe even randomize token valid duration
		if _, token, err := authjwt.GenerateAgentTokenValidFor(
			deploymentTarget.ID, deploymentTarget.OrganizationID, env.AgentTokenMaxValidDuration()); err != nil {
			log.Error("failed to create agent token", zap.Error(err))
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			_ = json.NewEncoder(w).Encode(api.AuthLoginResponse{Token: token})
		}
	}
}

func agentResourcesHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deploymentTarget := internalctx.GetDeploymentTarget(ctx)
	log := internalctx.GetLogger(ctx).With(zap.String("deploymentTargetId", deploymentTarget.ID.String()))

	var statusMessage string
	var appVersion *types.ApplicationVersion
	deployment, err := db.GetLatestDeploymentForDeploymentTarget(ctx, deploymentTarget.ID)
	if errors.Is(err, apierrors.ErrNotFound) {
		log.Info("latest deployment not found", zap.Error(err))
		statusMessage = "EMPTY"
	} else if err != nil {
		msg := "failed to get latest Deployment from DB"
		log.Error(msg, zap.Error(err))
		statusMessage = fmt.Sprintf("%v: %v", msg, err)
		w.WriteHeader(http.StatusInternalServerError)
	} else if av, err := db.GetApplicationVersion(ctx, deployment.ApplicationVersionID); err != nil {
		msg := "failed to get ApplicationVersion from DB"
		log.Error(msg, zap.Error(err))
		statusMessage = fmt.Sprintf("%v: %v", msg, err)
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		statusMessage = "OK"
		appVersion = av
	}

	var baseResource = api.AgentResource{Version: deploymentTarget.AgentVersion}
	var baseDeployment api.AgentDeployment

	if deployment != nil {
		baseDeployment.ID = deployment.ID
		baseDeployment.RevisionID = deployment.DeploymentRevisionID

		if deployment.ApplicationLicenseID != nil {
			if license, err := db.GetApplicationLicenseByID(ctx, *deployment.ApplicationLicenseID); err != nil {
				msg := "failed to get ApplicationLicense from DB"
				log.Error(msg, zap.Error(err))
				statusMessage = fmt.Sprintf("%v: %v", msg, err)
				w.WriteHeader(http.StatusInternalServerError)
			} else if license.RegistryURL != nil {
				baseDeployment.RegistryAuth = map[string]api.AgentRegistryAuth{
					*license.RegistryURL: {
						Username: *license.RegistryUsername,
						Password: *license.RegistryPassword,
					},
				}
			}
		}
	}

	// TODO: Consider consolidating all types into the same response format
	if deploymentTarget.Type == types.DeploymentTypeDocker {
		response := api.DockerAgentResource{AgentResource: baseResource}
		if deployment != nil && appVersion != nil {
			if composeYaml, err := appVersion.ParsedComposeFile(); err != nil {
				log.Warn("parse error", zap.Error(err))
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else if patchedComposeFile, err := patchProjectName(composeYaml, deployment.ID); err != nil {
				log.Warn("failed to patch project name", zap.Error(err))
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else {
				response.Deployment = &api.DockerAgentDeployment{
					AgentDeployment: baseDeployment,
					ComposeFile:     patchedComposeFile,
					EnvFile:         deployment.EnvFileData,
				}
			}
		} else {
			log.Debug("compose file is empty")
		}
		RespondJSON(w, response)
	} else {
		response := api.KubernetesAgentResource{AgentResource: baseResource, Namespace: *deploymentTarget.Namespace}
		if deployment != nil && appVersion != nil {
			response.Deployment = &api.KubernetesAgentDeployment{
				AgentDeployment: baseDeployment,
				ReleaseName:     *deployment.ReleaseName,
				ChartUrl:        *appVersion.ChartUrl,
				ChartVersion:    *appVersion.ChartVersion,
			}
			if versionValues, err := appVersion.ParsedValuesFile(); err != nil {
				log.Warn("parse error", zap.Error(err))
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else if deploymentValues, err := deployment.ParsedValuesFile(); err != nil {
				log.Warn("parse error", zap.Error(err))
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else if merged, err := util.MergeAllRecursive(versionValues, deploymentValues); err != nil {
				log.Warn("merge error", zap.Error(err))
				http.Error(w, fmt.Sprintf("error merging values files: %v", err), http.StatusInternalServerError)
				return
			} else {
				response.Deployment.Values = merged
			}
			if *appVersion.ChartType == types.HelmChartTypeRepository {
				response.Deployment.ChartName = *appVersion.ChartName
			}
		}
		RespondJSON(w, response)
	}

	// not in a TX because insertion should not be rolled back when the cleanup fails
	if err := db.CreateDeploymentTargetStatus(ctx, &deploymentTarget.DeploymentTarget, statusMessage); err != nil {
		log.Error("failed to create deployment target status – skipping cleanup of old statuses", zap.Error(err),
			zap.String("deploymentTargetId", deploymentTarget.ID.String()),
			zap.String("statusMessage", statusMessage))
	} else if cnt, err := db.CleanupDeploymentTargetStatus(ctx, &deploymentTarget.DeploymentTarget); err != nil {
		log.Error("failed to cleanup old deployment target status", zap.Error(err),
			zap.String("deploymentTargetId", deploymentTarget.ID.String()))
	} else if cnt > 0 {
		log.Debug("old deployment target statuses deleted",
			zap.String("deploymentTargetId", deploymentTarget.ID.String()),
			zap.Int64("count", cnt),
			zap.Duration("maxAge", *env.StatusEntriesMaxAge()))
	}
}

func patchProjectName(data map[string]any, deploymentID uuid.UUID) ([]byte, error) {
	if data == nil {
		data = make(map[string]any)
	}
	data["name"] = fmt.Sprintf("distr-%v", deploymentID.String()[:8])
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func angentPostStatusHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := internalctx.GetLogger(ctx)

	status, err := JsonBody[api.AgentDeploymentStatus](w, r)
	if err != nil {
		return
	}
	if err := db.CreateDeploymentRevisionStatus(ctx, status.RevisionID, status.Type, status.Message); err != nil {
		if errors.Is(err, apierrors.ErrConflict) {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		} else {
			log.Error("failed to create deployment revision status – skipping cleanup of old statuses", zap.Error(err),
				zap.Reflect("status", status))
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
	} else {
		w.WriteHeader(http.StatusOK)
	}

	// not in a TX because insertion should not be rolled back when the cleanup fails
	if cnt, err := db.CleanupDeploymentRevisionStatus(ctx, status.RevisionID); err != nil {
		log.Error("failed to cleanup old deployment revision status", zap.Error(err), zap.Reflect("status", status))
	} else if cnt > 0 {
		log.Debug("old deployment revision statuses deleted",
			zap.String("deploymentRevisionId", status.RevisionID.String()),
			zap.Int64("count", cnt),
			zap.Duration("maxAge", *env.StatusEntriesMaxAge()))
	}
}

func queryAuthDeploymentTargetCtxMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		targetID, err := uuid.Parse(r.URL.Query().Get("targetId"))
		if err != nil {
			http.Error(w, "targetId is not a valid UUID", http.StatusBadRequest)
			return
		}
		targetSecret := r.URL.Query().Get("targetSecret")

		if agentConnectPerTargetIdRateLimiter.RespondOnLimit(w, r, targetID.String()) {
			return
		} else if deploymentTarget, err := getVerifiedDeploymentTarget(ctx, targetID, targetSecret); err != nil {
			log.Error("failed to get deployment target from query auth", zap.Error(err))
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			ctx = internalctx.WithDeploymentTarget(ctx, deploymentTarget)
			next.ServeHTTP(w, r.WithContext(ctx))
		}
	})
}

func agentManifestHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		deploymentTarget := internalctx.GetDeploymentTarget(ctx)
		log := internalctx.GetLogger(ctx).With(zap.String("deploymentTargetId", deploymentTarget.ID.String()))

		if manifest, err := agentmanifest.Get(ctx, *deploymentTarget, nil); err != nil {
			log.Error("could not get agent manifest", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.Header().Add("Content-Type", "application/yaml")
			if _, err := io.Copy(w, manifest); err != nil {
				log.Warn("writing to client failed", zap.Error(err))
			}
		}
	}
}

func agentAuthDeploymentTargetCtxMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		auth := auth.AgentAuthentication.Require(ctx)
		orgId := auth.CurrentOrgID()
		targetId := auth.CurrentDeploymentTargetID()

		if deploymentTarget, err :=
			db.GetDeploymentTarget(ctx, targetId, &orgId); errors.Is(err, apierrors.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
		} else if err != nil {
			log.Error("failed to get DeploymentTarget", zap.Error(err))
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			if ua := r.UserAgent(); strings.HasPrefix(ua, fmt.Sprintf("%v/", useragent.DistrAgentUserAgent)) {
				reportedVersionName := strings.TrimPrefix(ua, fmt.Sprintf("%v/", useragent.DistrAgentUserAgent))
				if reportedVersion, err := db.GetAgentVersionWithName(ctx, reportedVersionName); err != nil {
					log.Error("could not get reported agent version", zap.Error(err))
					sentry.GetHubFromContext(ctx).CaptureException(err)
				} else if deploymentTarget.ReportedAgentVersionID == nil ||
					reportedVersion.ID != *deploymentTarget.ReportedAgentVersionID {
					if err := db.UpdateDeploymentTargetReportedAgentVersionID(
						ctx, deploymentTarget, reportedVersion.ID); err != nil {
						log.Error("could not update reported agent version", zap.Error(err))
						sentry.GetHubFromContext(ctx).CaptureException(err)
					}
				}
			}
			ctx = internalctx.WithDeploymentTarget(ctx, deploymentTarget)
			next.ServeHTTP(w, r.WithContext(ctx))
		}
	})
}

func getVerifiedDeploymentTarget(
	ctx context.Context,
	targetID uuid.UUID,
	targetSecret string,
) (*types.DeploymentTargetWithCreatedBy, error) {
	if deploymentTarget, err := db.GetDeploymentTarget(ctx, targetID, nil); err != nil {
		return nil, fmt.Errorf("failed to get deployment target from DB: %w", err)
	} else if deploymentTarget.AccessKeySalt == nil || deploymentTarget.AccessKeyHash == nil {
		return nil, errors.New("deployment target does not have key and salt")
	} else if err := security.VerifyAccessKey(
		*deploymentTarget.AccessKeySalt, *deploymentTarget.AccessKeyHash, targetSecret); err != nil {
		return nil, fmt.Errorf("failed to verify access: %w", err)
	} else {
		return deploymentTarget, nil
	}
}

var agentConnectPerTargetIdRateLimiter = httprate.NewRateLimiter(5, time.Minute)
var agentLoginPerTargetIdRateLimiter = httprate.NewRateLimiter(5, time.Minute)

var rateLimitPerAgent = httprate.Limit(
	2*15, // as long as we have 5 sec interval: 12 resources, 12 status requests
	1*time.Minute,
	httprate.WithKeyFuncs(middleware.RateLimitCurrentDeploymentTargetIdKeyFunc),
)
