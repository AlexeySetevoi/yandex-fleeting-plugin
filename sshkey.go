package yandex

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// setupSSHKey готовит ключ для доступа к инстансам группы. Отдельного ресурса
// «ключ» в Yandex Cloud нет: публичная часть уходит в metadata ssh-keys
// каждого инстанса. Для WinRM ничего не делает.
func (g *InstanceGroup) setupSSHKey() error {
	if g.settings.Protocol != provider.ProtocolSSH {
		return nil
	}

	var publicKey []byte

	if !g.settings.UseStaticCredentials {
		g.log.Info("generating ssh key")

		pub, priv, err := generateSSHKeyPair()
		if err != nil {
			return fmt.Errorf("could not generate ssh key pair: %w", err)
		}

		g.settings.Key = priv
		publicKey = pub
	} else if len(g.settings.Key) > 0 {
		g.log.Info("using static ssh key")

		pub, err := publicKeyFromPrivate(g.settings.Key)
		if err != nil {
			return fmt.Errorf("could not derive public key from static ssh key: %w", err)
		}

		publicKey = pub
	} else {
		// статический пароль или ключ, уже зашитый в образ
		return nil
	}

	g.sshKeysMetadata = g.settings.Username + ":" + strings.TrimSpace(string(publicKey))

	return nil
}

func generateSSHKeyPair() (publicKey, privateKey []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}

	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, nil, err
	}

	return ssh.MarshalAuthorizedKey(sshPub), pem.EncodeToMemory(block), nil
}

func publicKeyFromPrivate(privateKey []byte) ([]byte, error) {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	return ssh.MarshalAuthorizedKey(signer.PublicKey()), nil
}
