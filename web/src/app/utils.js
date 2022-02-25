import { rawEmojis} from "./emojis";

export const topicUrl = (baseUrl, topic) => `${baseUrl}/${topic}`;
export const topicUrlWs = (baseUrl, topic) => `${topicUrl(baseUrl, topic)}/ws`
    .replaceAll("https://", "wss://")
    .replaceAll("http://", "ws://");
export const topicUrlWsWithSince = (baseUrl, topic, since) => `${topicUrlWs(baseUrl, topic)}?since=${since}`;
export const topicUrlJson = (baseUrl, topic) => `${topicUrl(baseUrl, topic)}/json`;
export const topicUrlJsonPoll = (baseUrl, topic) => `${topicUrlJson(baseUrl, topic)}?poll=1`;
export const topicUrlAuth = (baseUrl, topic) => `${topicUrl(baseUrl, topic)}/auth`;
export const shortUrl = (url) => url.replaceAll(/https?:\/\//g, "");
export const shortTopicUrl = (baseUrl, topic) => shortUrl(topicUrl(baseUrl, topic));

// Format emojis (see emoji.js)
const emojis = {};
rawEmojis.forEach(emoji => {
    emoji.aliases.forEach(alias => {
        emojis[alias] = emoji.emoji;
    });
});

const toEmojis = (tags) => {
    if (!tags) return [];
    else return tags.filter(tag => tag in emojis).map(tag => emojis[tag]);
}

export const formatTitle = (m) => {
    const emojiList = toEmojis(m.tags);
    if (emojiList.length > 0) {
        return `${emojiList.join(" ")} ${m.title}`;
    } else {
        return m.title;
    }
};

export const formatMessage = (m) => {
    if (m.title) {
        return m.message;
    } else {
        const emojiList = toEmojis(m.tags);
        if (emojiList.length > 0) {
            return `${emojiList.join(" ")} ${m.message}`;
        } else {
            return m.message;
        }
    }
};

export const unmatchedTags = (tags) => {
    if (!tags) return [];
    else return tags.filter(tag => !(tag in emojis));
}

// From: https://developer.mozilla.org/en-US/docs/Web/API/Fetch_API/Using_Fetch
export async function* fetchLinesIterator(fileURL) {
    const utf8Decoder = new TextDecoder('utf-8');
    const response = await fetch(fileURL);
    const reader = response.body.getReader();
    let { value: chunk, done: readerDone } = await reader.read();
    chunk = chunk ? utf8Decoder.decode(chunk) : '';

    const re = /\n|\r|\r\n/gm;
    let startIndex = 0;

    for (;;) {
        let result = re.exec(chunk);
        if (!result) {
            if (readerDone) {
                break;
            }
            let remainder = chunk.substr(startIndex);
            ({ value: chunk, done: readerDone } = await reader.read());
            chunk = remainder + (chunk ? utf8Decoder.decode(chunk) : '');
            startIndex = re.lastIndex = 0;
            continue;
        }
        yield chunk.substring(startIndex, result.index);
        startIndex = re.lastIndex;
    }
    if (startIndex < chunk.length) {
        yield chunk.substr(startIndex); // last line didn't end in a newline char
    }
}
