// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package root

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elastic/beats/v7/libbeat/version"

	"github.com/elastic/apm-server/beater/auth"
	"github.com/elastic/apm-server/beater/beatertest"
)

func TestRootHandler(t *testing.T) {
	t.Run("404", func(t *testing.T) {
		c, w := beatertest.ContextWithResponseRecorder(http.MethodGet, "/abc/xyz")
		Handler(HandlerConfig{Version: "1.2.3"})(c)

		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Equal(t, `{"error":"404 page not found"}`+"\n", w.Body.String())
	})

	t.Run("ok", func(t *testing.T) {
		c, w := beatertest.ContextWithResponseRecorder(http.MethodGet, "/")
		Handler(HandlerConfig{Version: "1.2.3"})(c)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "", w.Body.String())
	})

	t.Run("unauthenticated", func(t *testing.T) {
		c, w := beatertest.ContextWithResponseRecorder(http.MethodGet, "/")
		c.Authentication.Method = auth.MethodAnonymous
		Handler(HandlerConfig{Version: "1.2.3"})(c)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "", w.Body.String())
	})

	t.Run("authenticated", func(t *testing.T) {
		c, w := beatertest.ContextWithResponseRecorder(http.MethodGet, "/")
		c.Authentication.Method = auth.MethodNone
		Handler(HandlerConfig{Version: "1.2.3"})(c)

		assert.Equal(t, http.StatusOK, w.Code)
		body := fmt.Sprintf("{\"build_date\":\"0001-01-01T00:00:00Z\",\"build_sha\":\"%s\",\"version\":\"1.2.3\"}\n",
			version.Commit())
		assert.Equal(t, body, w.Body.String())
	})

	t.Run("publish_ready", func(t *testing.T) {
		c, w := beatertest.ContextWithResponseRecorder(http.MethodGet, "/")
		c.Authentication.Method = auth.MethodNone

		Handler(HandlerConfig{
			PublishReady: func() bool { return false },
			Version:      "1.2.3",
		})(c)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, fmt.Sprintf(
			`{"build_date":"0001-01-01T00:00:00Z","build_sha":%q,"publish_ready":false,"version":"1.2.3"}`+"\n",
			version.Commit(),
		), w.Body.String())
	})
}
