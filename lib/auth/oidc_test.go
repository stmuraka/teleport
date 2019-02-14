/*
Copyright 2019 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"context"
	"fmt"
	"time"

	authority "github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/lite"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/coreos/go-oidc/oidc"
	"github.com/jonboulle/clockwork"
	"gopkg.in/check.v1"
)

type OIDCSuite struct {
	a *AuthServer
	b backend.Backend
	c clockwork.FakeClock
}

var _ = fmt.Printf
var _ = check.Suite(&OIDCSuite{})

func (s *OIDCSuite) SetUpSuite(c *check.C) {
	var err error

	utils.InitLoggerForTests()

	s.c = clockwork.NewFakeClockAt(time.Now())

	s.b, err = lite.NewWithConfig(context.Background(), lite.Config{
		Path:             c.MkDir(),
		PollStreamPeriod: 200 * time.Millisecond,
		Clock:            s.c,
	})
	c.Assert(err, check.IsNil)

	clusterName, err := services.NewClusterName(services.ClusterNameSpecV2{
		ClusterName: "me.localhost",
	})
	c.Assert(err, check.IsNil)

	authConfig := &InitConfig{
		ClusterName:            clusterName,
		Backend:                s.b,
		Authority:              authority.New(),
		SkipPeriodicOperations: true,
	}
	s.a, err = NewAuthServer(authConfig)
	c.Assert(err, check.IsNil)
}

func (s *OIDCSuite) TestCreateOIDCUser(c *check.C) {
	connector := services.NewOIDCConnector("oidcService", services.OIDCConnectorSpecV2{
		IssuerURL:    "https://www.example.com",
		ClientID:     "fakeClientID",
		ClientSecret: "fakeClientSecret",
		RedirectURL:  "https://www.example.com/redirect",
		Scope:        []string{"profile", "email"},
		ClaimsToRoles: []services.ClaimMapping{
			services.ClaimMapping{
				Claim: "email",
				Value: "foo@example.com",
				Roles: []string{"admin"},
			},
		},
	})

	ident := &oidc.Identity{
		Email:     "foo@example.com",
		ExpiresAt: s.c.Now().Add(1 * time.Minute),
	}

	claims := map[string]interface{}{
		"email": "foo@example.com",
	}

	// Create OIDC user with 1 minute expiry.
	err := s.a.createOIDCUser(connector, ident, claims)
	c.Assert(err, check.IsNil)

	// Within that 1 minute period the user should still exist.
	_, err = s.a.GetUser("foo@example.com")
	c.Assert(err, check.IsNil)

	// Advance time 2 minutes, the user should be gone.
	s.c.Advance(2 * time.Minute)
	_, err = s.a.GetUser("foo@example.com")
	c.Assert(err, check.NotNil)
}
