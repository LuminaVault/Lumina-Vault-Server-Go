# YouTube Video Cleanup Example

When ingesting YouTube videos using jina.ai, the raw output contains HTML artifacts, image placeholders, and JavaScript code. You need to manually clean up the content to create a readable markdown note.

## Raw jina.ai Output Issues

The raw output typically includes:

- Full HTML page structure
- Image placeholders (`[![Image X]()]`)
- Video player embeds and iframes
- JavaScript tracking code
- YouTube navigation elements
- Comment HTML structures

## Cleaned-Up Format

Transform the raw content into a structured markdown format like this:

```markdown
# Video Title

**YouTube Video:** [Watch here](https://www.youtube.com/watch?v=VIDEO_ID)

**Channel:** Channel Name

**Published:** Date

**Views:** View count

**Likes:** Like count

## Description
[Cleaned description text from the video]

## Comments

**Top Comments:**

- **@username:** Comment text ([like count] likes)
- **@username:** Comment text ([like count] likes)

## Key Topics Discussed

- Topic 1
- Topic 2
- Topic 3

## Video Links

- [Link text](https://www.youtube.com/watch?v=VIDEO_ID)
- [Link text](https://www.youtube.com/watch?v=VIDEO_ID)

## Follow the Channel

- **Subscribe:** [Channel link](https://www.youtube.com/channel/CHANNEL_ID)
- **Discord:** [Join here](DISCORD_LINK)
- **Twitter:** [@Handle](TWITTER_LINK)
- **Instagram:** [@Handle](INSTAGRAM_LINK)
- **Facebook:** [Page](FACEBOOK_LINK)
- **Store:** [Merch link](STORE_LINK)
```

## Example: Trump Is About To Crash The Global Economy

This is a cleaned-up version of the YouTube video ingestion from May 10, 2026:

```markdown
# Trump Is About To Crash The Global Economy

**YouTube Video:** [Watch here](https://www.youtube.com/watch?v=-5q5qUA41s0)

**Channel:** The Majority Report w/ Sam Seder

**Published:** May 9, 2026

**Views:** 142,421

**Likes:** 4.7K

## Description
Trump Is About To Crash The Global Economy

Watch the Majority Report live Monday–Friday at 12pm EST on YouTube or [http://www.Majority.fm](https://www.youtube.com/redirect?event=video_description&redir_token=QUFFLUhqbFhoejVjb1NNVkVQVXhxWnVXQ3N3TXJqODFmZ3xBQ3Jtc0tuQTFFRFdzM3JzVGN3c01aeTFQUFdPTHMtM0JNUV9TMjVMaVIzWkxLRjlMdlhwdlVvWWRRQndxZlpkWFpUNDMxMlh2ZkJXTmZxN0JiNm5CWEczUVNFMzJjZVpJaFltYUg0SDV1T01mYkwxR05aNnNBbw&q=http%3A%2F%2Fwww.Majority.fm%2F&v=-5q5qUA41s0) To connect and organize with your local ICE rapid response team visit ICERRT.com The Congress switchboard number is (202) 224-3121. You can use this number to connect with either the US Senate or the House of Representatives. Send us IM messages during the live-stream with our free Majority Report App: [http://majority.fm/app](https://www.youtube.com/redirect?event=video_description&redir_token=QUFFLUhqbld6ZlNKQWRmUHlPVjFwT0d5Rk5waHE5YnMyUXxBQ3Jtc0ttSGQyOEFlZDdLWlQtQXRKRGVpSkZoZEVCU24tekZ6X1FMdmxuZnVyT2h5dGdHbW9tcksxWDh0MWtRdjQ4bWVFaTJHYTJHa3JkaUtfWFp4bDBDb0QyajRXZWhNTEt6QXk1ZzVNZTNwZFBHc29WOXZuNA&q=http%3A%2F%2Fmajority.fm%2Fapp&v=-5q5qUA41s0) Subscribe to MR's daily AM Quickie newsletter: [https://am-quickie.ghost.io](https://www.youtube.com/redirect?event=video_description&redir_token=QUFFLUhqbU5McWRGN25Oajhpd2ZlV2hkeDZGYUpxUEY3Z3xBQ3Jtc0ttWWh2bXhXZl8yTTA5Q2xScUFYcWdQc3kyc3ZBQUFsX0IxOEdINV9LTi1YSy1uWExFLXJPSzl4RHhjNVJ4OXpVa29XR2ljTXRsOHdfOXF1TE5yeVYyT1VQUFNqUWFTTGIzanRETU5tdFc5WXgyTHJ1VQ&q=http%3A%2F%2Fam-quickie.ghost.io%2F&v=-5q5qUA41s0) Find all your MR merch at our store: [https://shop.majorityreportradio.com](https://www.youtube.com/redirect?event=video_description&redir_token=QUFFLUhqbDA5ZkZYZWVNTDRVTlRWRWh5ZWJ2d2hmVE5Cd3xBQ3Jtc0tsZXA4bjVLcnMxRzBLNmljamE0c2E5OTNia0hUWFF1YUhtdzdPSVVyMXZUNDd4bTVsZ3NjcVVIZ3llX1RIeXpkQ1ZWWWZiaTdhcWVfOTMwRkVsZlFEN2tLclRzdk9teUZITGVSVG53Zmg3MzFsSmZHTQ&q=https%3A%2F%2Fshop.majorityreportradio.com%2F&v=-5q5qUA41s0) Become a Majority Report member: [https://fans.fm/majority/join](https://www.youtube.com/redirect?event=video_description&redir_token=QUFFLUhqa1F3NkZrODRSOU5US0NrbW1KTEhkU29vWVFfUXxBQ3Jtc0ttRTRXeHZUMGpUTFBnMFRxLTc1RkM0YW5aRlZURjdEZHV0anNSb0NEdGZCa3lXNWc3Z045Rnpid3ZGNjNRQnJqeklpNHhCRWhvcjZyMHV0TEdnV2d4RFJpSWpjeFpWYUdESjZTSDZQXzFGc1EtbmVXbw&q=http%3A%2F%2Ffans.fm%2Fmajority%2Fjoin&v=-5q5qUA41s0)

## Comments

**Top Comments:**

- **@09spidy:** "We're gonna have a great depression, the greatest the world has ever seen" (690 likes)
- **@PontiusPilatesPublishing:** "It's almost like an economic system built on glorifying greed and human selfishness doesn't end up working that well in the long run. How unexpected" (753 likes)
- **@BittyDuckbill:** "The fact the stock market is still going up is proof the system is broken." (357 likes)
- **@mnoir7257:** "We should've put his ass in prison five years ago" (394 likes)
- **@mihau5037:** "\"I voted for Trump because of the economy\" are words I've heard countless times over a year ago." (374 likes)

## Key Topics Discussed

- Trump's economic policies and their global impact
- Potential market crash and recession
- Oil price shocks and negotiations with oil execs
- Trump's business history of bankruptcies
- Impact on everyday Americans ("my pockets are already in a recession")
- Political blame games and media narratives

## Video Links

- [What SCOTUS Voting Rights Act Decision Means | Ari Berman | TMR](https://www.youtube.com/watch?v=FBxZVDbB3YA)
- [Trump Is Trapped by The Majority Report w/ Sam Seder](https://www.youtube.com/watch?v=Twq24m3JxYc)
- [Building Political Solidarity | Yuh-Line Niou | TMR](https://www.youtube.com/watch?v=4atQwOqMdCY)
- [The Coming Oil Shock | Rory Johnston | TMR](https://www.youtube.com/watch?v=WN1PJoqeKhI)
- And more in the description

## Follow the Channel

- **Subscribe:** [The Majority Report](https://www.youtube.com/@TheMajorityReport)
- **Podcast:** [Listen here](https://www.youtube.com/podcasts)
- **Discord:** [Join here](https://discord.com/invite/majority)
- **Twitter:** [@MajorityFM](https://twitter.com/MajorityFM)
- **Instagram:** [@majorityreport.fm](https://www.instagram.com/majorityreport.fm)
- **Facebook:** [MajorityReport](https://www.facebook.com/MajorityReport)
- **Store:** [Shop Majority Report merch](https://shop.majorityreportradio.com)

## Cleanup Process

1. Extract the raw content using jina.ai
2. Remove HTML structure and JavaScript code
3. Extract the video description and comments
4. Format as markdown with proper frontmatter
5. Save to appropriate topic directory (News, Tech, etc.)
```

## Key Takeaways

- YouTube videos require manual cleanup after jina.ai extraction
- Focus on extracting human-readable content: description, comments, links
- Structure the note with clear sections and metadata
- Save to topic-specific directories based on content
- Use the cleaned-up example as a template for future YouTube ingestions