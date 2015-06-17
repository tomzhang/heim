package proto

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"encoding/json"

	"euphoria.io/heim/proto/security"
	"euphoria.io/heim/proto/snowflake"
	"euphoria.io/scope"
)

type clientKey int

type Client struct {
	IP            string
	UserAgent     string
	Connected     time.Time
	Agent         *Agent
	Account       Account
	Authorization Authorization
}

func (c *Client) FromRequest(ctx scope.Context, r *http.Request) {
	c.UserAgent = r.Header.Get("User-Agent")
	c.Connected = time.Now()
	c.IP = getIP(r)

	var k clientKey
	ctx.Set(k, c)
}

func (c *Client) FromContext(ctx scope.Context) bool {
	var k clientKey
	src, ok := ctx.Get(k).(*Client)
	if !ok || src == nil {
		return false
	}
	*c = *src
	return true
}

func (c *Client) UserID() string {
	if c.Account != nil {
		return fmt.Sprintf("account:%s", c.Account.ID().String())
	}
	return fmt.Sprintf("agent:%s", c.Agent.IDString())
}

func (c *Client) AuthenticateWithAgent(
	ctx scope.Context, backend Backend, room Room, agent *Agent, agentKey *security.ManagedKey) error {

	if agent.AccountID == "" {
		return nil
	}

	var accountID snowflake.Snowflake
	if err := accountID.FromString(agent.AccountID); err != nil {
		return err
	}

	account, err := backend.GetAccount(ctx, accountID)
	if err != nil {
		if err == ErrAccountNotFound {
			return nil
		}
		return err
	}

	clientKey, err := agent.Unlock(agentKey)
	if err != nil {
		return fmt.Errorf("agent key error: %s", err)
	}

	c.Account = account
	c.Authorization.ClientKey = clientKey

	holderKey, err := account.Unlock(clientKey)
	if err != nil {
		return fmt.Errorf("client key error: %s", err)
	}

	// TODO: check if account is manager

	// Look for message key grants to this account.
	messageKey, err := room.MessageKey(ctx)
	if err != nil {
		return err
	}
	if messageKey != nil {
		managerKey, err := room.ManagerKey(ctx)
		if err != nil {
			return err
		}
		subjectKey := managerKey.KeyPair()
		capabilityID := security.PublicKeyCapabilityID(&subjectKey, holderKey, messageKey.Nonce())
		capability, err := room.GetCapability(ctx, capabilityID)
		if err != nil {
			return err
		}
		if capability != nil {
			pkc := &security.PublicKeyCapability{
				Capability: capability,
			}
			roomKeyJSON, err := pkc.DecryptPayload(&subjectKey, holderKey)
			if err != nil {
				return err
			}
			roomKey := &security.ManagedKey{
				KeyType: security.AES128,
			}
			if err := json.Unmarshal(roomKeyJSON, &roomKey.Plaintext); err != nil {
				return err
			}
			c.Authorization.AddMessageKey(messageKey.KeyID(), roomKey)
		}
	}

	return nil
}

func (c *Client) AuthenticateWithPasscode(ctx scope.Context, room Room, passcode string) (string, error) {
	mkey, err := room.MessageKey(ctx)
	if err != nil {
		return "", err
	}

	if mkey == nil {
		return "", nil
	}

	holderKey := security.KeyFromPasscode([]byte(passcode), mkey.Nonce(), security.AES128)

	capabilityID, err := security.SharedSecretCapabilityID(holderKey, mkey.Nonce())
	if err != nil {
		return "", err
	}

	capability, err := room.GetCapability(ctx, capabilityID)
	if err != nil {
		return "", err
	}

	if capability == nil {
		return "passcode incorrect", nil
	}

	roomKey, err := decryptRoomKey(holderKey, capability)
	if err != nil {
		return "", err
	}

	// TODO: convert to account grant if signed in
	// TODO: load and return all historic keys

	c.Authorization.AddMessageKey(mkey.KeyID(), roomKey)
	return "", nil
}

func getIP(r *http.Request) string {
	addr := r.RemoteAddr
	if ffs := r.Header["X-Forwarded-For"]; len(ffs) > 0 {
		parts := strings.Split(ffs[len(ffs)-1], ",")
		addr = strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
