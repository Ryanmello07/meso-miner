package connect

// api_wallet_consent.go — the coldkey challenge behind a signed subnet wallet
// set (sn/PLAN.md 7.3, decision D-2).
//
// `POST /sn/wallet` accepts a coldkey signature over a server-issued challenge
// instead of trusting the caller's login proof. The challenge itself comes from
// `POST /auth/wallet-challenge`, the same public route the ur.io wallet bridge
// uses, and it is deliberately unauthenticated: the signature is what proves
// possession, so the challenge must be obtainable before any credential is
// attached.
//
// Follows the api_verify.go idiom: an Args struct, a Result struct, a Callback
// alias, an async method, and a blocking Sync method.

import "fmt"

type AuthWalletChallengeCallback ApiCallback[*AuthWalletChallengeResult]

// AuthWalletChallengeArgs asks for the message a coldkey signs.
//
// Blockchain is the wallet's chain family; Purpose is informational and the
// server does not pin it. The subnet wallet set sends PurposeConnect so the
// request matches the wallet bridge's.
type AuthWalletChallengeArgs struct {
	WalletAddress string `json:"wallet_address,omitempty"`
	Blockchain    string `json:"blockchain,omitempty"`
	Purpose       string `json:"purpose,omitempty"`
}

// PurposeConnect marks a challenge that proves a subnet claim wallet
// (`POST /sn/wallet`) rather than a sign-in.
const PurposeConnect = "connect"

// BlockchainTao is the Bittensor chain family, the only one a subnet coldkey
// can have.
const BlockchainTao = "TAO"

// AuthWalletChallengeResult is the server's challenge for one address.
//
// MessageTemplate holds the exact UTF-8 bytes to sign, LF line endings and no
// trailing newline; its exact signed form is retained and replayed with the
// wallet set, so a lost response can still be retried.
type AuthWalletChallengeResult struct {
	Challenge       string                    `json:"challenge,omitempty"`
	Timestamp       int64                     `json:"timestamp,omitempty"`
	ExpiresIn       int64                     `json:"expires_in,omitempty"`
	MessageTemplate string                    `json:"message_template,omitempty"`
	Error           *AuthWalletChallengeError `json:"error,omitempty"`
}

// AuthWalletChallengeError is the server's refusal of a challenge request.
// Like SnSetWalletError, the platform sends it as HTTP 200 with an error
// object, so it has to be read out of the body rather than from the status.
type AuthWalletChallengeError struct {
	Message string `json:"message"`
}

// AuthWalletChallenge requests a challenge message for an address to sign, via
// the public `POST /auth/wallet-challenge` route. A jwt is sent when one is
// set, but the route does not require it.
func (self *BringYourApi) AuthWalletChallenge(authWalletChallenge *AuthWalletChallengeArgs, callback AuthWalletChallengeCallback) {
	go HandleError(func() {
		HttpPostWithStrategy(
			self.ctx,
			self.clientStrategy,
			fmt.Sprintf("%s/auth/wallet-challenge", self.apiUrl),
			authWalletChallenge,
			self.ByJwt(),
			&AuthWalletChallengeResult{},
			callback,
		)
	})
}

func (self *BringYourApi) AuthWalletChallengeSync(authWalletChallenge *AuthWalletChallengeArgs) (*AuthWalletChallengeResult, error) {
	return HttpPostWithStrategy(
		self.ctx,
		self.clientStrategy,
		fmt.Sprintf("%s/auth/wallet-challenge", self.apiUrl),
		authWalletChallenge,
		self.ByJwt(),
		&AuthWalletChallengeResult{},
		NewNoopApiCallback[*AuthWalletChallengeResult](),
	)
}
