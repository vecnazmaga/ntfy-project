// THIS FILE IS JUST AN EXAMPLE
//
// It is removed during the build process. The actual config is dynamically
// generated server-side and served by the ntfy server.
//
// During web development, you may change values here for rapid testing.

var config = {
    base_url: "http://127.0.0.1:2586",// FIXME window.location.origin, // Change to test against a different server
    app_root: "/app",
    enable_login: true,
    enable_signup: true,
    enable_payments: true,
    enable_reservations: true,
    enable_sms: true,
    enable_calls: true,
    billing_contact: "",
    disallowed_topics: ["docs", "static", "file", "app", "account", "settings", "signup", "login", "v1"]
};
