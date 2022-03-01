import * as React from 'react';
import {useEffect, useState} from 'react';
import Box from '@mui/material/Box';
import {ThemeProvider} from '@mui/material/styles';
import CssBaseline from '@mui/material/CssBaseline';
import Toolbar from '@mui/material/Toolbar';
import Notifications from "./Notifications";
import theme from "./theme";
import api from "../app/Api";
import repository from "../app/Repository";
import connectionManager from "../app/ConnectionManager";
import Subscriptions from "../app/Subscriptions";
import Navigation from "./Navigation";
import ActionBar from "./ActionBar";
import Users from "../app/Users";
import notificationManager from "../app/NotificationManager";
import NoTopics from "./NoTopics";
import Preferences from "./Preferences";

// TODO subscribe dialog:
//  - check/use existing user
//  - add baseUrl
// TODO user management
// TODO embed into ntfy server
// TODO make default server functional
// TODO indexeddb for notifications + subscriptions
// TODO business logic with callbacks

const App = () => {
    console.log(`[App] Rendering main view`);

    const [mobileDrawerOpen, setMobileDrawerOpen] = useState(false);
    const [prefsOpen, setPrefsOpen] = useState(false);
    const [subscriptions, setSubscriptions] = useState(new Subscriptions());
    const [users, setUsers] = useState(new Users());
    const [selectedSubscription, setSelectedSubscription] = useState(null);
    const [notificationsGranted, setNotificationsGranted] = useState(notificationManager.granted());
    const handleSubscriptionClick = (subscriptionId) => {
        setSelectedSubscription(subscriptions.get(subscriptionId));
        setPrefsOpen(false);
    }
    const handleSubscribeSubmit = (subscription, user) => {
        console.log(`[App] New subscription: ${subscription.id}`);
        if (user !== null) {
            setUsers(prev => prev.add(user).clone());
        }
        setSubscriptions(prev => prev.add(subscription).clone());
        setSelectedSubscription(subscription);
        poll(subscription, user);
        handleRequestPermission();
    };
    const handleDeleteNotification = (subscriptionId, notificationId) => {
        console.log(`[App] Deleting notification ${notificationId} from ${subscriptionId}`);
        setSubscriptions(prev => {
            const newSubscription = prev.get(subscriptionId).deleteNotification(notificationId);
            return prev.update(newSubscription).clone();
        });
    };
    const handleDeleteAllNotifications = (subscriptionId) => {
        console.log(`[App] Deleting all notifications from ${subscriptionId}`);
        setSubscriptions(prev => {
            const newSubscription = prev.get(subscriptionId).deleteAllNotifications();
            return prev.update(newSubscription).clone();
        });
    };
    const handleUnsubscribe = (subscriptionId) => {
        console.log(`[App] Unsubscribing from ${subscriptionId}`);
        setSubscriptions(prev => {
            const newSubscriptions = prev.remove(subscriptionId).clone();
            setSelectedSubscription(newSubscriptions.firstOrNull());
            return newSubscriptions;
        });
    };
    const handleRequestPermission = () => {
        notificationManager.maybeRequestPermission((granted) => {
            setNotificationsGranted(granted);
        })
    };
    const handlePrefsClick = () => {
        setPrefsOpen(true);
        setSelectedSubscription(null);
    };
    const poll = (subscription, user) => {
        const since = subscription.last;
        api.poll(subscription.baseUrl, subscription.topic, since, user)
            .then(notifications => {
                setSubscriptions(prev => {
                    subscription.addNotifications(notifications);
                    return prev.update(subscription).clone();
                });
            });
    };

    // Define hooks: Note that the order of the hooks is important. The "loading" hooks
    // must be before the "saving" hooks.
    useEffect(() => {
        // Load subscriptions and users
        const subscriptions = repository.loadSubscriptions();
        const selectedSubscriptionId = repository.loadSelectedSubscriptionId();
        const users = repository.loadUsers();
        setSubscriptions(subscriptions);
        setUsers(users);

        // Set selected subscription
        const maybeSelectedSubscription = subscriptions.get(selectedSubscriptionId);
        if (maybeSelectedSubscription) {
            setSelectedSubscription(maybeSelectedSubscription);
        }

        // Poll all subscriptions
        subscriptions.forEach((subscriptionId, subscription) => {
            const user = users.get(subscription.baseUrl); // May be null
            poll(subscription, user);
        });
    }, [/* initial render */]);
    useEffect(() => {
        const notificationClickFallback = (subscription) => setSelectedSubscription(subscription);
        const handleNotification = (subscriptionId, notification) => {
            setSubscriptions(prev => {
                const subscription = prev.get(subscriptionId);
                if (subscription.addNotification(notification)) {
                    notificationManager.notify(subscription, notification, notificationClickFallback)
                }
                return prev.update(subscription).clone();
            });
        };
        connectionManager.refresh(subscriptions, users, handleNotification);
    }, [subscriptions, users]);
    useEffect(() => repository.saveSubscriptions(subscriptions), [subscriptions]);
    useEffect(() => repository.saveUsers(users), [users]);
    useEffect(() => {
        const subscriptionId = (selectedSubscription) ? selectedSubscription.id : "";
        repository.saveSelectedSubscriptionId(subscriptionId)
    }, [selectedSubscription]);

    return (
        <ThemeProvider theme={theme}>
            <CssBaseline/>
            <Box sx={{display: 'flex'}}>
                <CssBaseline/>
                <ActionBar
                    selectedSubscription={selectedSubscription}
                    users={users}
                    onClearAll={handleDeleteAllNotifications}
                    onUnsubscribe={handleUnsubscribe}
                    onMobileDrawerToggle={() => setMobileDrawerOpen(!mobileDrawerOpen)}
                />
                <Box component="nav" sx={{width: {sm: Navigation.width}, flexShrink: {sm: 0}}}>
                    <Navigation
                        subscriptions={subscriptions}
                        selectedSubscription={selectedSubscription}
                        mobileDrawerOpen={mobileDrawerOpen}
                        notificationsGranted={notificationsGranted}
                        prefsOpen={prefsOpen}
                        onMobileDrawerToggle={() => setMobileDrawerOpen(!mobileDrawerOpen)}
                        onSubscriptionClick={handleSubscriptionClick}
                        onSubscribeSubmit={handleSubscribeSubmit}
                        onPrefsClick={handlePrefsClick}
                        onRequestPermissionClick={handleRequestPermission}
                    />
                </Box>
                <Box
                    component="main"
                    sx={{
                        display: 'flex',
                        flexGrow: 1,
                        flexDirection: 'column',
                        padding: 3,
                        width: {sm: `calc(100% - ${Navigation.width}px)`},
                        height: '100vh',
                        overflow: 'auto',
                        backgroundColor: (theme) => theme.palette.mode === 'light' ? theme.palette.grey[100] : theme.palette.grey[900]
                    }}
                >
                    <Toolbar/>
                    <MainContent
                        subscription={selectedSubscription}
                        prefsOpen={prefsOpen}
                        onDeleteNotification={handleDeleteNotification}
                    />
                </Box>
            </Box>
        </ThemeProvider>
    );
}

const MainContent = (props) => {
    if (props.prefsOpen) {
        return <Preferences/>;
    }
    if (props.subscription !== null) {
        return (
            <Notifications
                subscription={props.subscription}
                onDeleteNotification={props.onDeleteNotification}
            />
        );
    } else {
        return <NoTopics/>;
    }
};

export default App;
