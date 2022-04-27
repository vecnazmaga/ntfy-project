package server

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestParseActions(t *testing.T) {
	actions, err := parseActions("[]")
	require.Nil(t, err)
	require.Empty(t, actions)

	// Basic test
	actions, err = parseActions("action=http, label=Open door, url=https://door.lan/open; view, Show portal, https://door.lan")
	require.Nil(t, err)
	require.Equal(t, 2, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, "Open door", actions[0].Label)
	require.Equal(t, "https://door.lan/open", actions[0].URL)
	require.Equal(t, "view", actions[1].Action)
	require.Equal(t, "Show portal", actions[1].Label)
	require.Equal(t, "https://door.lan", actions[1].URL)

	// JSON
	actions, err = parseActions(`[{"action":"http","label":"Open door","url":"https://door.lan/open"}, {"action":"view","label":"Show portal","url":"https://door.lan"}]`)
	require.Nil(t, err)
	require.Equal(t, 2, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, "Open door", actions[0].Label)
	require.Equal(t, "https://door.lan/open", actions[0].URL)
	require.Equal(t, "view", actions[1].Action)
	require.Equal(t, "Show portal", actions[1].Label)
	require.Equal(t, "https://door.lan", actions[1].URL)

	// Other params
	actions, err = parseActions("action=http, label=Open door, url=https://door.lan/open, body=this is a body, method=PUT")
	require.Nil(t, err)
	require.Equal(t, 1, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, "Open door", actions[0].Label)
	require.Equal(t, "https://door.lan/open", actions[0].URL)
	require.Equal(t, "PUT", actions[0].Method)
	require.Equal(t, "this is a body", actions[0].Body)

	// Extras with underscores
	actions, err = parseActions("action=broadcast, label=Do a thing, extras.command=some command, extras.some_param=a parameter")
	require.Nil(t, err)
	require.Equal(t, 1, len(actions))
	require.Equal(t, "broadcast", actions[0].Action)
	require.Equal(t, "Do a thing", actions[0].Label)
	require.Equal(t, 2, len(actions[0].Extras))
	require.Equal(t, "some command", actions[0].Extras["command"])
	require.Equal(t, "a parameter", actions[0].Extras["some_param"])

	// Headers with dashes
	actions, err = parseActions("action=http, label=Send request, url=http://example.com, method=GET, headers.Content-Type=application/json, headers.Authorization=Basic sdasffsf")
	require.Nil(t, err)
	require.Equal(t, 1, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, "Send request", actions[0].Label)
	require.Equal(t, 2, len(actions[0].Headers))
	require.Equal(t, "application/json", actions[0].Headers["Content-Type"])
	require.Equal(t, "Basic sdasffsf", actions[0].Headers["Authorization"])

	// Quotes
	actions, err = parseActions(`action=http, "Look ma, \"quotes\"; and semicolons", url=http://example.com`)
	require.Nil(t, err)
	require.Equal(t, 1, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, `Look ma, \"quotes\"; and semicolons`, actions[0].Label)
	require.Equal(t, `http://example.com`, actions[0].URL)

	// Single quotes
	actions, err = parseActions(`action=http, '"quotes" and \'single quotes\'', url=http://example.com`)
	require.Nil(t, err)
	require.Equal(t, 1, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, `"quotes" and \'single quotes\'`, actions[0].Label)
	require.Equal(t, `http://example.com`, actions[0].URL)

	// Out of order
	actions, err = parseActions(`label="Out of order!" , action="http", url=http://example.com`)
	require.Nil(t, err)
	require.Equal(t, 1, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, `Out of order!`, actions[0].Label)
	require.Equal(t, `http://example.com`, actions[0].URL)

	// Spaces
	actions, err = parseActions(`action = http, label = 'this is a label', url = "http://google.com"`)
	require.Nil(t, err)
	require.Equal(t, 1, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, `this is a label`, actions[0].Label)
	require.Equal(t, `http://google.com`, actions[0].URL)

	// Non-ASCII
	actions, err = parseActions(`action = http, 'Кохайтеся а не воюйте, 💙🫤', url = "http://google.com"`)
	require.Nil(t, err)
	require.Equal(t, 1, len(actions))
	require.Equal(t, "http", actions[0].Action)
	require.Equal(t, `Кохайтеся а не воюйте, 💙🫤`, actions[0].Label)
	require.Equal(t, `http://google.com`, actions[0].URL)

	// Invalid syntax
	actions, err = parseActions(`label="Out of order!" x, action="http", url=http://example.com`)
	require.EqualError(t, err, "unexpected character 'x' at position 22")

	actions, err = parseActions(`label="", action="http", url=http://example.com`)
	require.EqualError(t, err, "parameter 'label' is required")

	actions, err = parseActions(`label=, action="http", url=http://example.com`)
	require.EqualError(t, err, "parameter 'label' is required")

	actions, err = parseActions(`label="xx", action="http", url=http://example.com, what is this anyway`)
	require.EqualError(t, err, "term 'what is this anyway' unknown")

	actions, err = parseActions(`fdsfdsf`)
	require.EqualError(t, err, "action 'fdsfdsf' unknown")

	actions, err = parseActions(`aaa=a, "bbb, 'ccc, ddd, eee "`)
	require.EqualError(t, err, "key 'aaa' unknown")

	actions, err = parseActions(`action=http, label="omg the end quote is missing`)
	require.EqualError(t, err, "unexpected end of input, quote started at position 20")
}
