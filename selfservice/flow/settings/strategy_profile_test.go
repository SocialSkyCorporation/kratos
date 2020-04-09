package settings_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/julienschmidt/httprouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/ory/x/urlx"

	"github.com/ory/x/pointerx"

	"github.com/ory/x/httpx"

	"github.com/ory/viper"

	"github.com/ory/kratos/driver/configuration"
	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/internal"
	"github.com/ory/kratos/internal/httpclient/client/common"
	"github.com/ory/kratos/internal/httpclient/models"
	"github.com/ory/kratos/internal/testhelpers"
	"github.com/ory/kratos/selfservice/flow/settings"
	"github.com/ory/kratos/selfservice/form"
	"github.com/ory/kratos/x"
)

func TestStrategyTraits(t *testing.T) {
	_, reg := internal.NewRegistryDefault(t)
	viper.Set(configuration.ViperKeyDefaultIdentityTraitsSchemaURL, "file://./stub/identity.schema.json")

	ui := testhelpers.NewSettingsUITestServer(t)
	viper.Set(configuration.ViperKeySelfServicePrivilegedAuthenticationAfter, "1ns")

	primaryIdentity := &identity.Identity{
		ID: x.NewUUID(),
		Credentials: map[identity.CredentialsType]identity.Credentials{
			"password": {Type: "password", Identifiers: []string{"john@doe.com"}, Config: json.RawMessage(`{"hashed_password":"foo"}`)},
		},
		Traits:         identity.Traits(`{"email":"john@doe.com","stringy":"foobar","booly":false,"numby":2.5,"should_long_string":"asdfasdfasdfasdfasfdasdfasdfasdf","should_big_number":2048}`),
		TraitsSchemaID: configuration.DefaultIdentityTraitsSchemaID,
	}
	publicTS, adminTS := testhelpers.NewSettingsAPIServer(t, reg, []identity.Identity{
		*primaryIdentity, {ID: x.NewUUID(), Traits: identity.Traits(`{}`)}})

	primaryUser := testhelpers.NewSessionClient(t, publicTS.URL+"/sessions/set/0")
	publicClient := testhelpers.NewSDKClient(publicTS)
	adminClient := testhelpers.NewSDKClient(adminTS)

	t.Run("description=not authorized to call endpoints without a session", func(t *testing.T) {
		pr, ar := x.NewRouterPublic(), x.NewRouterAdmin()
		reg.SettingsStrategies().RegisterPublicRoutes(pr)
		reg.SettingsHandler().RegisterPublicRoutes(pr)
		reg.SettingsHandler().RegisterAdminRoutes(ar)

		adminTS, publicTS := httptest.NewServer(ar), httptest.NewServer(pr)
		defer adminTS.Close()
		defer publicTS.Close()

		for k, tc := range []*http.Request{
			httpx.MustNewRequest("POST", publicTS.URL+settings.PublicSettingsProfilePath, strings.NewReader(url.Values{"foo": {"bar"}}.Encode()), "application/x-www-form-urlencoded"),
			httpx.MustNewRequest("POST", publicTS.URL+settings.PublicSettingsProfilePath, strings.NewReader(`{"foo":"bar"}`), "application/json"),
		} {
			t.Run(fmt.Sprintf("case=%d", k), func(t *testing.T) {
				res, err := http.DefaultClient.Do(tc)
				require.NoError(t, err)
				defer res.Body.Close()
				out, err := ioutil.ReadAll(res.Body)
				require.NoError(t, err)
				assert.EqualValues(t, http.StatusUnauthorized, res.StatusCode, "%s", out)
			})
		}
	})

	t.Run("daemon=public", func(t *testing.T) {
		t.Run("description=should fail to post data if CSRF is missing", func(t *testing.T) {
			f := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)
			res, err := primaryUser.PostForm(pointerx.StringR(f.Action), url.Values{})
			require.NoError(t, err)
			assert.EqualValues(t, 400, res.StatusCode, "should return a 400 error because CSRF token is not set")
		})

		t.Run("description=should redirect to settings management ui and /settings/requests?request=... should come back with the right information", func(t *testing.T) {
			res, err := primaryUser.Get(publicTS.URL + settings.PublicPath)
			require.NoError(t, err)

			assert.Equal(t, ui.URL, res.Request.URL.Scheme+"://"+res.Request.URL.Host)
			assert.Equal(t, "/settings", res.Request.URL.Path, "should end up at the profile URL")

			rid := res.Request.URL.Query().Get("request")
			require.NotEmpty(t, rid)

			pr, err := publicClient.Common.GetSelfServiceBrowserSettingsRequest(
				common.NewGetSelfServiceBrowserSettingsRequestParams().WithHTTPClient(primaryUser).WithRequest(rid),
			)
			require.NoError(t, err, "%s", rid)

			assert.Equal(t, rid, string(pr.Payload.ID))
			assert.NotEmpty(t, pr.Payload.Identity)
			assert.Equal(t, primaryIdentity.ID.String(), string(pr.Payload.Identity.ID))
			assert.JSONEq(t, string(primaryIdentity.Traits), x.MustEncodeJSON(t, pr.Payload.Identity.Traits))
			assert.Equal(t, primaryIdentity.TraitsSchemaID, pointerx.StringR(pr.Payload.Identity.TraitsSchemaID))
			assert.Equal(t, publicTS.URL+settings.PublicPath, pointerx.StringR(pr.Payload.RequestURL))

			found := false

			require.NotNil(t, pr.Payload.Methods[settings.StrategyTraitsID].Config)
			f := pr.Payload.Methods[settings.StrategyTraitsID].Config

			for i := range f.Fields {
				if pointerx.StringR(f.Fields[i].Name) == form.CSRFTokenName {
					found = true
					require.NotEmpty(t, f.Fields[i])
					f.Fields = append(f.Fields[:i], f.Fields[i+1:]...)
					break
				}
			}
			require.True(t, found)

			assert.EqualValues(t, &models.Form{
				Action: pointerx.String(publicTS.URL + settings.PublicSettingsProfilePath + "?request=" + rid),
				Method: pointerx.String("POST"),
				Fields: models.FormFields{
					&models.FormField{Name: pointerx.String("traits.email"), Type: pointerx.String("text"), Value: "john@doe.com"},
					&models.FormField{Name: pointerx.String("traits.stringy"), Type: pointerx.String("text"), Value: "foobar"},
					&models.FormField{Name: pointerx.String("traits.numby"), Type: pointerx.String("number"), Value: json.Number("2.5")},
					&models.FormField{Name: pointerx.String("traits.booly"), Type: pointerx.String("checkbox"), Value: false},
					&models.FormField{Name: pointerx.String("traits.should_big_number"), Type: pointerx.String("number"), Value: json.Number("2048")},
					&models.FormField{Name: pointerx.String("traits.should_long_string"), Type: pointerx.String("text"), Value: "asdfasdfasdfasdfasfdasdfasdfasdf"},
				},
			}, f)
		})

		t.Run("description=should come back with form errors if some profile data is invalid", func(t *testing.T) {
			config := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)

			values := testhelpers.SDKFormFieldsToURLValues(config.Fields)
			values.Set("traits.should_long_string", "too-short")
			values.Set("traits.stringy", "bazbar") // it should still override new values!
			actual, _ := testhelpers.SettingsSubmitForm(t, config, primaryUser, values)

			assert.NotEmpty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==csrf_token).value").String(), "%s", actual)
			assert.Equal(t, "too-short", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_long_string).value").String(), "%s", actual)
			assert.Equal(t, "bazbar", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.stringy).value").String(), "%s", actual)
			assert.Equal(t, "2.5", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.numby).value").String(), "%s", actual)
			assert.Equal(t, "length must be >= 25, but got 9", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_long_string).errors.0.message").String(), "%s", actual)
		})

		t.Run("description=should update protected field with sudo mode", func(t *testing.T) {
			var called int
			loginTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, 0, called)
				called++

				viper.Set(configuration.ViperKeySelfServicePrivilegedAuthenticationAfter, "5m")
				t.Cleanup(func() {
					viper.Set(configuration.ViperKeySelfServicePrivilegedAuthenticationAfter, "1ns")
				})

				res, err := adminClient.Common.GetSelfServiceBrowserLoginRequest(common.NewGetSelfServiceBrowserLoginRequestParams().WithRequest(r.URL.Query().Get("request")))
				require.NoError(t, err)
				require.NotEmpty(t, res.Payload.RequestURL)

				redir := urlx.ParseOrPanic(*res.Payload.RequestURL).Query().Get("return_to")
				t.Logf("Redirecting to: %s", redir)
				http.Redirect(w, r, redir, http.StatusFound)
			}))
			defer loginTS.Close()
			viper.Set(configuration.ViperKeyURLsLogin, loginTS.URL+"/login")

			config := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)
			newEmail := "not-john-doe@mail.com"
			values := testhelpers.SDKFormFieldsToURLValues(config.Fields)
			values.Set("traits.email", newEmail)
			actual, response := testhelpers.SettingsSubmitForm(t, config, primaryUser, values)
			assert.True(t, pointerx.BoolR(response.Payload.UpdateSuccessful), "%s", actual)

			assert.Empty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.numby).errors").Value(), "%s", actual)
			assert.Empty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_big_number).errors").Value(), "%s", actual)
			assert.Empty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_long_string).errors").Value(), "%s", actual)

			assert.Equal(t, newEmail, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.email).value").Value(), "%s", actual)

			assert.Equal(t, "foobar", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.stringy).value").String(), "%s", actual) // sanity check if original payload is still here
		})

		t.Run("description=should end up at the login endpoint if trying to update protected field without sudo mode", func(t *testing.T) {
			loginTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, r.URL.Path, "/login")
				_, _ = w.Write([]byte("called login page"))
			}))
			viper.Set(configuration.ViperKeyURLsLogin, loginTS.URL+"/login")

			var run = func(t *testing.T) {
				f := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)

				values := testhelpers.SDKFormFieldsToURLValues(f.Fields)
				values.Set("traits.email", "not-john-doe")
				res, err := primaryUser.PostForm(pointerx.StringR(f.Action), values)
				require.NoError(t, err)
				defer res.Body.Close()

				body, err := ioutil.ReadAll(res.Body)
				require.NoError(t, err)
				assert.EqualValues(t, string(body), "called login page", "%s", body)
			}

			t.Run("case=should fail without hooks", run)

			t.Run("case=should fail with hooks", func(t *testing.T) {
				testhelpers.SetSettingsStrategyAfterHooks(t, settings.StrategyTraitsID, publicTS.URL+"/return-ts")
				t.Cleanup(func() {
					viper.Set(configuration.ViperKeySelfServiceSettingsAfterConfig+"."+settings.StrategyTraitsID, nil)
				})
				run(t)
			})
		})

		t.Run("description=should retry with invalid payloads multiple times before succeeding", func(t *testing.T) {
			t.Run("flow=fail first update", func(t *testing.T) {
				f := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)

				values := testhelpers.SDKFormFieldsToURLValues(f.Fields)
				values.Set("traits.should_big_number", "1")
				actual, response := testhelpers.SettingsSubmitForm(t, f, primaryUser, values)
				assert.False(t, pointerx.BoolR(response.Payload.UpdateSuccessful), "%s", actual)

				assert.Equal(t, "1", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_big_number).value").String(), "%s", actual)
				assert.Equal(t, "must be >= 1200 but found 1", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_big_number).errors.0.message").String(), "%s", actual)

				assert.Equal(t, "foobar", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.stringy).value").String(), "%s", actual) // sanity check if original payload is still here
			})

			t.Run("flow=fail second update", func(t *testing.T) {
				f := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)

				values := testhelpers.SDKFormFieldsToURLValues(f.Fields)
				values.Del("traits.should_big_number")
				values.Set("traits.should_long_string", "short")
				values.Set("traits.numby", "this-is-not-a-number")
				actual, response := testhelpers.SettingsSubmitForm(t, f, primaryUser, values)
				assert.False(t, pointerx.BoolR(response.Payload.UpdateSuccessful), "%s", actual)

				assert.Empty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_big_number).errors.0.message").String(), "%s", actual)
				assert.Empty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_big_number).value").String(), "%s", actual)

				assert.Equal(t, "short", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_long_string).value").String(), "%s", actual)
				assert.Equal(t, "length must be >= 25, but got 5", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_long_string).errors.0.message").String(), "%s", actual)

				assert.Equal(t, "this-is-not-a-number", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.numby).value").String(), "%s", actual)
				assert.Equal(t, "expected number, but got string", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.numby).errors.0.message").String(), "%s", actual)

				assert.Equal(t, "foobar", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.stringy).value").String(), "%s", actual) // sanity check if original payload is still here
			})

			t.Run("flow=succeed with final request", func(t *testing.T) {
				f := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)

				values := testhelpers.SDKFormFieldsToURLValues(f.Fields)
				// set email to the one that is in the db as it should not be modified
				values.Set("traits.email", "not-john-doe@mail.com")
				values.Set("traits.numby", "15")
				values.Set("traits.should_big_number", "9001")
				values.Set("traits.should_long_string", "this is such a long string, amazing stuff!")
				actual, response := testhelpers.SettingsSubmitForm(t, f, primaryUser, values)
				assert.True(t, pointerx.BoolR(response.Payload.UpdateSuccessful), "%s", actual)

				assert.Empty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.numby).errors").Value(), "%s", actual)
				assert.Empty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_big_number).errors").Value(), "%s", actual)
				assert.Empty(t, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_long_string).errors").Value(), "%s", actual)

				assert.Equal(t, 15.0, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.numby).value").Value(), "%s", actual)
				assert.Equal(t, 9001.0, gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_big_number).value").Value(), "%s", actual)
				assert.Equal(t, "this is such a long string, amazing stuff!", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.should_long_string).value").Value(), "%s", actual)

				assert.Equal(t, "foobar", gjson.Get(actual, "methods.profile.config.fields.#(name==traits.stringy).value").String(), "%s", actual) // sanity check if original payload is still here
			})

			t.Run("flow=try another update with invalid data", func(t *testing.T) {
				f := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)

				values := testhelpers.SDKFormFieldsToURLValues(f.Fields)
				values.Set("traits.should_long_string", "short")
				actual, response := testhelpers.SettingsSubmitForm(t, f, primaryUser, values)
				assert.False(t, pointerx.BoolR(response.Payload.UpdateSuccessful), "%s", actual)
			})
		})
	})

	t.Run("description=ensure that hooks are running", func(t *testing.T) {
		var returned bool
		router := httprouter.New()
		router.GET("/return-ts", func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
			returned = true
		})
		rts := httptest.NewServer(router)
		defer rts.Close()

		viper.Set(configuration.ViperKeySelfServiceSettingsAfterConfig+"."+settings.StrategyTraitsID, testhelpers.HookConfigRedirectTo(t, rts.URL+"/return-ts"))

		f := testhelpers.GetSettingsMethodConfig(t, primaryUser, publicTS, settings.StrategyTraitsID)

		values := testhelpers.SDKFormFieldsToURLValues(f.Fields)
		values.Set("traits.should_big_number", "9001")
		res, err := primaryUser.PostForm(pointerx.StringR(f.Action), values)

		require.NoError(t, err)
		defer res.Body.Close()

		body, err := ioutil.ReadAll(res.Body)
		require.NoError(t, err)
		assert.True(t, returned, "%d - %s", res.StatusCode, body)
	})
}