package handler

import (
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/secretsmanager"
	"github.com/google/go-github/github"
	"github.com/sirupsen/logrus"
)

// New lambda handler with the provided settings.
func New(manager *Manager, tokenTemplate, keyTemplate, titleTemplate string, logger *logrus.Logger) func(Team) error {
	return func(team Team) error {
		tokenAdded := make(map[string]bool)

	Loop:
		for _, repository := range team.Repositories {
			log := logger.WithFields(logrus.Fields{
				"team":       team.Name,
				"repository": repository.Name,
				"owner":      repository.Owner,
			})

			tokenPath, err := NewTemplate(team.Name, repository.Name, repository.Owner, tokenTemplate).String()
			if err != nil {
				log.Warnf("failed to parse token path template: %s", err)
				continue
			}

			keyPath, err := NewTemplate(team.Name, repository.Name, repository.Owner, keyTemplate).String()
			if err != nil {
				log.Warnf("failed to parse deploy key template: %s", err)
				continue
			}

			title, err := NewTemplate(team.Name, repository.Name, repository.Owner, titleTemplate).String()
			if err != nil {
				log.Warnf("failed to github title template: %s", err)
				continue
			}

			// Write an access token for the organisation
			if _, ok := tokenAdded[repository.Owner]; !ok {
				token, err := manager.createAccessToken(repository.Owner)
				if err != nil {
					log.Warnf("failed to get access token: %s", err)
					continue
				}
				if err := manager.writeSecret(tokenPath, token); err != nil {
					log.Warnf("failed to write access token: %s", err)
					continue
				}
				tokenAdded[repository.Owner] = true
			}

			// Look for existing keys belongning to the team
			keys, err := manager.listKeys(repository)
			if err != nil {
				log.Warnf("failed to list github keys: %s", err)
				continue
			}

			var oldKey *github.Key
			for _, key := range keys {
				if *key.Title == title {
					oldKey = key

					// Rotate the key if read/write permissions have changed
					if key.ReadOnly != nil && *key.ReadOnly != bool(repository.ReadOnly) {
						break
					}
					// Do not rotate if nothing has changed and the key is not >7 days old
					updated, err := manager.getLastUpdated(keyPath)
					if err != nil {
						if e, ok := err.(awserr.Error); ok && e.Code() == secretsmanager.ErrCodeResourceNotFoundException {
							// Do not log a warning if we fail to describe because the secret does not exist.
							break
						}
						log.Warnf("failed to get last updated for secret: %s", err)
						break
					}
					if updated.After(time.Now().AddDate(0, 0, -7)) {
						continue Loop
					}
				}
			}

			// Generate a new key pair
			private, public, err := manager.generateKeyPair(title)
			if err != nil {
				log.Warnf("failed to generate new key pair: %s", err)
				continue
			}

			// Write the new public key to Github
			if err = manager.createKey(repository, title, public); err != nil {
				log.Warnf("failed to create key on github: %s", err)
				continue
			}

			// Write the private key to Secrets manager
			if err := manager.writeSecret(keyPath, private); err != nil {
				log.Warnf("failed to write secret key: %s", err)
				continue
			}

			// Sleep before deleting old key (in case someone has just fetched the old key)
			if oldKey != nil {
				time.Sleep(time.Second * 1)
				if err = manager.deleteKey(repository, *oldKey.ID); err != nil {
					log.Warnf("failed to delete old github key: %d: %s", *oldKey.ID, err)
					continue
				}
			}

		}
		return nil
	}
}
