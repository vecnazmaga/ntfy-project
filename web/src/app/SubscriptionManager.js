import api from "./Api";
import notifier from "./Notifier";
import prefs from "./Prefs";
import getDb from "./getDb";
import { topicUrl } from "./utils";

class SubscriptionManager {
  constructor(db) {
    this.db = db;
  }

  /** All subscriptions, including "new count"; this is a JOIN, see https://dexie.org/docs/API-Reference#joining */
  async all() {
    const subscriptions = await this.db.subscriptions.toArray();
    return Promise.all(
      subscriptions.map(async (s) => ({
        ...s,
        new: await this.db.notifications.where({ subscriptionId: s.id, new: 1 }).count(),
      }))
    );
  }

  async webPushTopics() {
    // the Promise.resolve wrapper is not superfluous, without it the live query breaks:
    // https://dexie.org/docs/dexie-react-hooks/useLiveQuery()#calling-non-dexie-apis-from-querier
    if (!(await Promise.resolve(notifier.pushEnabled()))) {
      return [];
    }

    const subscriptions = await this.db.subscriptions.where({ mutedUntil: 0, baseUrl: config.base_url }).toArray();

    // internal is currently a bool, it could be a 0/1 to be indexable, but for now just filter them out here
    return subscriptions.filter(({ internal }) => !internal).map(({ topic }) => topic);
  }

  async get(subscriptionId) {
    return this.db.subscriptions.get(subscriptionId);
  }

  async notify(subscriptionId, notification, defaultClickAction) {
    const subscription = await this.get(subscriptionId);

    if (subscription.mutedUntil === 1) {
      return;
    }

    const priority = notification.priority ?? 3;
    if (priority < (await prefs.minPriority())) {
      return;
    }

    await Promise.all([notifier.playSound(), notifier.notify(subscription, notification, defaultClickAction)]);
  }

  /**
   * @param {string} baseUrl
   * @param {string} topic
   * @param {object} opts
   * @param {boolean} opts.internal
   * @returns
   */
  async add(baseUrl, topic, opts = {}) {
    const id = topicUrl(baseUrl, topic);

    const existingSubscription = await this.get(id);
    if (existingSubscription) {
      return existingSubscription;
    }

    const subscription = {
      ...opts,
      id: topicUrl(baseUrl, topic),
      baseUrl,
      topic,
      mutedUntil: 0,
      last: null,
    };

    await this.db.subscriptions.put(subscription);

    return subscription;
  }

  async syncFromRemote(remoteSubscriptions, remoteReservations) {
    console.log(`[SubscriptionManager] Syncing subscriptions from remote`, remoteSubscriptions);

    // Add remote subscriptions
    const remoteIds = await Promise.all(
      remoteSubscriptions.map(async (remote) => {
        const reservation = remoteReservations?.find((r) => remote.base_url === config.base_url && remote.topic === r.topic) || null;

        const local = await this.add(remote.base_url, remote.topic, {
          displayName: remote.display_name, // May be undefined
          reservation, // May be null!
        });

        return local.id;
      })
    );

    // Remove local subscriptions that do not exist remotely
    const localSubscriptions = await this.db.subscriptions.toArray();

    await Promise.all(
      localSubscriptions.map(async (local) => {
        const remoteExists = remoteIds.includes(local.id);
        if (!local.internal && !remoteExists) {
          await this.remove(local);
        }
      })
    );
  }

  async refreshWebPushSubscriptions(presetTopics) {
    const topics = presetTopics ?? (await this.webPushTopics());

    await api.updateWebPushSubscriptions(topics, await notifier.getBrowserSubscription());
  }

  async updateState(subscriptionId, state) {
    this.db.subscriptions.update(subscriptionId, { state });
  }

  async remove(subscription) {
    await this.db.subscriptions.delete(subscription.id);
    await this.db.notifications.where({ subscriptionId: subscription.id }).delete();
  }

  async first() {
    return this.db.subscriptions.toCollection().first(); // May be undefined
  }

  async getNotifications(subscriptionId) {
    // This is quite awkward, but it is the recommended approach as per the Dexie docs.
    // It's actually fine, because the reading and filtering is quite fast. The rendering is what's
    // killing performance. See  https://dexie.org/docs/Collection/Collection.offset()#a-better-paging-approach

    return this.db.notifications
      .orderBy("time") // Sort by time first
      .filter((n) => n.subscriptionId === subscriptionId)
      .reverse()
      .toArray();
  }

  async getAllNotifications() {
    return this.db.notifications
      .orderBy("time") // Efficient, see docs
      .reverse()
      .toArray();
  }

  /** Adds notification, or returns false if it already exists */
  async addNotification(subscriptionId, notification) {
    const exists = await this.db.notifications.get(notification.id);
    if (exists) {
      return false;
    }
    try {
      // sw.js duplicates this logic, so if you change it here, change it there too
      await this.db.notifications.add({
        ...notification,
        subscriptionId,
        // New marker (used for bubble indicator); cannot be boolean; Dexie index limitation
        new: 1,
      }); // FIXME consider put() for double tab
      await this.db.subscriptions.update(subscriptionId, {
        last: notification.id,
      });
    } catch (e) {
      console.error(`[SubscriptionManager] Error adding notification`, e);
    }
    return true;
  }

  /** Adds/replaces notifications, will not throw if they exist */
  async addNotifications(subscriptionId, notifications) {
    const notificationsWithSubscriptionId = notifications.map((notification) => ({ ...notification, subscriptionId }));
    const lastNotificationId = notifications.at(-1).id;
    await this.db.notifications.bulkPut(notificationsWithSubscriptionId);
    await this.db.subscriptions.update(subscriptionId, {
      last: lastNotificationId,
    });
  }

  async updateNotification(notification) {
    const exists = await this.db.notifications.get(notification.id);
    if (!exists) {
      return false;
    }
    try {
      await this.db.notifications.put({ ...notification });
    } catch (e) {
      console.error(`[SubscriptionManager] Error updating notification`, e);
    }
    return true;
  }

  async deleteNotification(notificationId) {
    await this.db.notifications.delete(notificationId);
  }

  async deleteNotifications(subscriptionId) {
    await this.db.notifications.where({ subscriptionId }).delete();
  }

  async markNotificationRead(notificationId) {
    await this.db.notifications.where({ id: notificationId }).modify({ new: 0 });
  }

  async markNotificationsRead(subscriptionId) {
    await this.db.notifications.where({ subscriptionId, new: 1 }).modify({ new: 0 });
  }

  async setMutedUntil(subscriptionId, mutedUntil) {
    await this.db.subscriptions.update(subscriptionId, {
      mutedUntil,
    });
  }

  async setDisplayName(subscriptionId, displayName) {
    await this.db.subscriptions.update(subscriptionId, {
      displayName,
    });
  }

  async setReservation(subscriptionId, reservation) {
    await this.db.subscriptions.update(subscriptionId, {
      reservation,
    });
  }

  async update(subscriptionId, params) {
    await this.db.subscriptions.update(subscriptionId, params);
  }

  async pruneNotifications(thresholdTimestamp) {
    await this.db.notifications.where("time").below(thresholdTimestamp).delete();
  }
}

export default new SubscriptionManager(getDb());
