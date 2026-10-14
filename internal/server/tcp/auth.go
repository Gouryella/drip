package tcp

import "drip/internal/shared/utils"

const (
	authFailureCode    = "authentication_failed"
	authFailureMessage = "Invalid authentication token"
)

func isAuthTokenAccepted(providedToken, configuredToken string, allowAnonymous bool) bool {
	if providedToken == "" {
		return allowAnonymous
	}
	if configuredToken == "" {
		return false
	}

	return utils.ConstantTimeEqualString(providedToken, configuredToken)
}
