"""
discord_bot.py
--------------
Discord bot that listens on RabbitMQ for `tts.ready` events and plays
the audio file into the voice channel it's currently in.

Commands:
  !join   — bot joins the voice channel of the user who issued the command
            (gated to REQUIRED_ROLE)

RabbitMQ:
  Exchange: discord_events (fanout)
  Event type: tts.ready
  Payload: { "path": "/absolute/path/to/file.mp3" }

.env keys:
  DISCORD_TOKEN
  DISCORD_GUILD_ID
  RABBITMQ_URL
  DISCORD_EXCHANGE      (default: discord_events)
  DISCORD_QUEUE         (default: discord_tts)
  DISCORD_REQUIRED_ROLE (default: DM)
"""

import asyncio
import json
import os
import threading

import discord
import pika
from discord.ext import commands
from dotenv import load_dotenv

load_dotenv()

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
DISCORD_TOKEN    = os.getenv("DISCORD_TOKEN")
DISCORD_GUILD_ID = int(os.getenv("DISCORD_GUILD_ID", 0))
RABBITMQ_URL     = os.getenv("RABBITMQ_URL")
EXCHANGE         = os.getenv("DISCORD_EXCHANGE", "discord_events")
QUEUE_NAME       = os.getenv("DISCORD_QUEUE", "discord_tts")
REQUIRED_ROLE    = os.getenv("DISCORD_REQUIRED_ROLE", "DM")

# ---------------------------------------------------------------------------
# Bot setup
# ---------------------------------------------------------------------------
intents = discord.Intents.default()
intents.message_content = True
intents.voice_states = True

bot = commands.Bot(command_prefix="!", intents=intents)

# ---------------------------------------------------------------------------
# !join command
# ---------------------------------------------------------------------------
@bot.command(name="join")
async def join(ctx: commands.Context):
    # Role gate
    role_names = [r.name for r in ctx.author.roles]
    if REQUIRED_ROLE not in role_names:
        await ctx.send(f"You need the `{REQUIRED_ROLE}` role to do that.")
        print(f"[Discord] !join rejected — {ctx.author} does not have role '{REQUIRED_ROLE}'")
        return

    # Must be in a voice channel
    if not ctx.author.voice or not ctx.author.voice.channel:
        await ctx.send("You need to be in a voice channel first.")
        print(f"[Discord] !join rejected — {ctx.author} is not in a voice channel")
        return

    channel = ctx.author.voice.channel

    # Already connected — move if different channel
    if ctx.voice_client:
        if ctx.voice_client.channel == channel:
            await ctx.send(f"Already in {channel.name}.")
            return
        await ctx.voice_client.move_to(channel)
        await ctx.send(f"Moved to {channel.name}.")
        print(f"[Discord] Moved to voice channel: {channel.name}")
    else:
        await channel.connect()
        await ctx.send(f"Joined {channel.name}.")
        print(f"[Discord] Joined voice channel: {channel.name}")

@bot.command(name="leave")
async def leave(ctx: commands.Context):
    role_names = [r.name for r in ctx.author.roles]
    if REQUIRED_ROLE not in role_names:
        await ctx.send(f"You need the `{REQUIRED_ROLE}` role to do that.")
        return

    if not ctx.voice_client or not ctx.voice_client.is_connected():
        await ctx.send("I'm not in a voice channel.")
        return

    await ctx.voice_client.disconnect()
    await ctx.send("Left the voice channel.")
    print(f"[Discord] Left voice channel on request from {ctx.author}")
    

# ---------------------------------------------------------------------------
# Audio playback
# ---------------------------------------------------------------------------
async def play_audio(path: str) -> None:
    """Play an audio file into the current voice channel."""
    guild = bot.get_guild(DISCORD_GUILD_ID)
    if not guild:
        print(f"[Discord] Guild {DISCORD_GUILD_ID} not found.")
        return

    vc = guild.voice_client
    if not vc or not vc.is_connected():
        print(f"[Discord] Not in a voice channel — skipping playback of {path}")
        return

    if not os.path.exists(path):
        print(f"[Discord] Audio file not found: {path}")
        return

    if vc.is_playing():
        print(f"[Discord] Already playing audio — queuing is not implemented, skipping {path}")
        return

    print(f"[Discord] Playing: {path}")
    source = discord.FFmpegOpusAudio(path)
    vc.play(source, after=lambda e: print(f"[Discord] Playback complete: {path}" if not e else f"[Discord] Playback error: {e}"))


# ---------------------------------------------------------------------------
# RabbitMQ consumer — runs in a background thread
# ---------------------------------------------------------------------------
def _rabbitmq_consumer(loop: asyncio.AbstractEventLoop) -> None:
    """
    Blocking RabbitMQ consumer. Runs in its own thread.
    Submits play_audio() coroutines back onto the bot's event loop.
    """
    while True:
        try:
            params = pika.URLParameters(RABBITMQ_URL)
            conn = pika.BlockingConnection(params)
            ch = conn.channel()
            ch.exchange_declare(exchange=EXCHANGE, exchange_type="fanout", durable=True)
            ch.queue_declare(queue=QUEUE_NAME, durable=True)
            ch.queue_bind(exchange=EXCHANGE, queue=QUEUE_NAME)

            print(f"[Discord] RabbitMQ consumer ready — exchange: {EXCHANGE}, queue: {QUEUE_NAME}")

            def callback(ch_inner, method, properties, body):
                event_type = properties.type or "unknown"
                print(f"[Discord] Received event: {event_type}")

                if event_type != "tts.ready":
                    ch_inner.basic_ack(delivery_tag=method.delivery_tag)
                    return

                try:
                    payload = json.loads(body.decode())
                except json.JSONDecodeError:
                    print(f"[Discord] Failed to decode payload: {body}")
                    ch_inner.basic_ack(delivery_tag=method.delivery_tag)
                    return

                path = payload.get("path")
                if not path:
                    print(f"[Discord] tts.ready event missing 'path' field")
                    ch_inner.basic_ack(delivery_tag=method.delivery_tag)
                    return

                print(f"[Discord] tts.ready received — path: {path}")
                asyncio.run_coroutine_threadsafe(play_audio(path), loop)
                ch_inner.basic_ack(delivery_tag=method.delivery_tag)

            ch.basic_consume(queue=QUEUE_NAME, on_message_callback=callback, auto_ack=False)
            ch.start_consuming()

        except Exception as e:
            print(f"[Discord] RabbitMQ error: {e} — retrying in 5s...")
            import time
            time.sleep(5)


# ---------------------------------------------------------------------------
# Bot events
# ---------------------------------------------------------------------------
@bot.event
async def on_ready():
    print(f"[Discord] Logged in as {bot.user} ({bot.user.id})")
    print(f"[Discord] Guild ID: {DISCORD_GUILD_ID}")
    print(f"[Discord] Required role for !join: {REQUIRED_ROLE}")
    print(f"[Discord] Listening for tts.ready on exchange: {EXCHANGE}")

    # Start RabbitMQ consumer in background thread once bot is ready
    loop = asyncio.get_event_loop()
    thread = threading.Thread(target=_rabbitmq_consumer, args=(loop,), daemon=True)
    thread.start()

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
def main():
    print("=" * 50)
    print("  Discord TTS Bot")
    print("=" * 50)
    print(f"  Exchange:      {EXCHANGE}")
    print(f"  Queue:         {QUEUE_NAME}")
    print(f"  Required role: {REQUIRED_ROLE}")
    print()
    bot.run(DISCORD_TOKEN)


if __name__ == "__main__":
    main()
    