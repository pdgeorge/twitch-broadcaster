"""Tests for the receiver's channel-point reward calls (Dabi's shop), against a
fake Twitch: these are the calls that create, switch and refund real rewards on
the channel, so a mistake would show up in chat's rewards menu.

Run from twitch_receiver/, with requirements.txt installed: python -m unittest discover tests
"""
import itertools
import logging
import unittest
from pathlib import Path

from aiohttp import web
from aiohttp.test_utils import TestServer

from src import main as receiver

# Every request and reward change logs at INFO, which buries a failure.
logging.getLogger("aiohttp.access").setLevel(logging.WARNING)
logging.getLogger("asyncio").setLevel(logging.WARNING)
receiver.LOGGER.setLevel(logging.WARNING)


class FakeTwitch:
    """Just enough of Helix's custom-reward endpoints. foreign holds titles of
    rewards made by hand in the dashboard, which the bot can't see or change."""

    def __init__(self):
        self.rewards = {}  # id -> the bot's own rewards
        self.foreign = set()
        self.calls = []
        self.redemptions = []
        self.fail_with = None  # (status, message) for every request
        self.ids = itertools.count(1)

    def app(self):
        app = web.Application()
        app.router.add_get("/helix/channel_points/custom_rewards", self.list)
        app.router.add_post("/helix/channel_points/custom_rewards", self.create)
        app.router.add_patch("/helix/channel_points/custom_rewards", self.update)
        app.router.add_patch("/helix/channel_points/custom_rewards/redemptions", self.redemption)
        return app

    def failure(self):
        """The canned error, if there is one. Compared with None, not used as a
        bool: an aiohttp response with nothing stored on it is falsy."""
        if self.fail_with:
            status, message = self.fail_with
            return web.json_response({"status": status, "message": message}, status=status)
        return None

    async def list(self, request):
        self.calls.append(("GET", None))
        assert request.query["only_manageable_rewards"] == "true"
        if (failure := self.failure()) is not None:
            return failure
        return web.json_response({"data": list(self.rewards.values())})

    @staticmethod
    def store(reward, body):
        """Keep body's fields the way Helix returns them: the per-user-per-stream
        limit is set with flat fields but read back nested."""
        body = dict(body)
        setting = reward.setdefault("max_per_user_per_stream_setting", {"is_enabled": False, "max_per_user_per_stream": 0})
        if "is_max_per_user_per_stream_enabled" in body:
            setting["is_enabled"] = body.pop("is_max_per_user_per_stream_enabled")
        if "max_per_user_per_stream" in body:
            setting["max_per_user_per_stream"] = body.pop("max_per_user_per_stream")
        reward.update(body)

    async def create(self, request):
        body = await request.json()
        self.calls.append(("POST", body))
        if (failure := self.failure()) is not None:
            return failure
        titles = self.foreign | {r["title"].lower() for r in self.rewards.values()}
        if body["title"].lower() in titles:
            return web.json_response({"status": 400, "message": "CREATE_CUSTOM_REWARD_DUPLICATE_REWARD"}, status=400)
        reward = {"id": f"r{next(self.ids)}"}
        self.store(reward, body)
        self.rewards[reward["id"]] = reward
        return web.json_response({"data": [reward]})

    async def update(self, request):
        body = await request.json()
        self.calls.append(("PATCH", body))
        if (failure := self.failure()) is not None:
            return failure
        reward = self.rewards[request.query["id"]]
        self.store(reward, body)
        return web.json_response({"data": [reward]})

    async def redemption(self, request):
        body = await request.json()
        self.redemptions.append((dict(request.query), body["status"]))
        if (failure := self.failure()) is not None:
            return failure
        return web.json_response({"data": []})


def spec(box, cost=300, enabled=True):
    return {"title": f"Buy item {box}", "cost": cost, "prompt": f"box {box}", "enabled": enabled, "background_color": "#E8338E", "max_per_user_per_stream": 3}


class RewardTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.twitch = FakeTwitch()
        self.server = TestServer(self.twitch.app())
        await self.server.start_server()
        base = str(self.server.make_url("/helix"))
        self.saved = (receiver.TWITCH_REWARDS_URL, receiver.TWITCH_REDEMPTIONS_URL)
        receiver.TWITCH_REWARDS_URL = f"{base}/channel_points/custom_rewards"
        receiver.TWITCH_REDEMPTIONS_URL = f"{base}/channel_points/custom_rewards/redemptions"
        tokens = receiver.TokenManager("client", "secret", "refresh", Path("unused.json"))
        tokens.access_token = "token"
        self.client = receiver.TwitchEventSubClient(tokens, "100", None)

    async def asyncTearDown(self):
        receiver.TWITCH_REWARDS_URL, receiver.TWITCH_REDEMPTIONS_URL = self.saved
        if self.client.session:
            await self.client.session.close()
        await self.server.close()

    def writes(self):
        return [call for call in self.twitch.calls if call[0] != "GET"]

    async def test_creates_rewards_only_when_they_should_be_on(self):
        await self.client.sync_rewards([spec(1, 450), spec(2, enabled=False), spec(3)], seq=1)
        created = {body["title"]: body for method, body in self.writes() if method == "POST"}
        self.assertEqual(set(created), {"Buy item 1", "Buy item 3"})
        self.assertEqual(created["Buy item 1"]["cost"], 450)
        self.assertTrue(created["Buy item 1"]["is_enabled"])
        self.assertFalse(created["Buy item 1"]["should_redemptions_skip_request_queue"], "skipping the queue would make them unrefundable")
        self.assertEqual(created["Buy item 1"]["background_color"], "#E8338E")
        made = next(r for r in self.twitch.rewards.values() if r["title"] == "Buy item 1")
        self.assertEqual(made["max_per_user_per_stream_setting"], {"is_enabled": True, "max_per_user_per_stream": 3})

    async def test_updates_only_what_changed(self):
        await self.client.sync_rewards([spec(1), spec(2), spec(3)], seq=1)
        self.twitch.calls.clear()
        await self.client.sync_rewards([spec(1), spec(2), spec(3)], seq=2)
        self.assertEqual(self.writes(), [], "an unchanged reward was written again")

        await self.client.sync_rewards([spec(1, enabled=False), spec(2, cost=900), spec(3)], seq=3)
        patched = [body for method, body in self.writes() if method == "PATCH"]
        self.assertEqual(sorted((b["title"], b["is_enabled"], b["cost"]) for b in patched), [("Buy item 1", False, 300), ("Buy item 2", True, 900)])
        self.assertEqual(len(self.twitch.rewards), 3, "a sync made a second copy of a reward")

    async def test_closing_never_creates_rewards(self):
        await self.client.sync_rewards([spec(1, enabled=False), spec(2, enabled=False), spec(3, enabled=False)], seq=1)
        self.assertEqual(self.writes(), [])

    async def test_a_late_older_sync_is_dropped(self):
        await self.client.sync_rewards([spec(1, enabled=False)], seq=10)
        self.twitch.calls.clear()
        await self.client.sync_rewards([spec(1)], seq=5)
        self.assertEqual(self.twitch.calls, [], "an older sync ran after a newer one")

    async def test_a_hand_made_reward_with_the_same_title(self):
        self.twitch.foreign.add("buy item 1")
        with self.assertRaisesRegex(RuntimeError, "wasn't made by this bot"):
            await self.client.sync_rewards([spec(1), spec(2)], seq=1)
        self.assertEqual([r["title"] for r in self.twitch.rewards.values()], ["Buy item 2"], "one bad reward stopped the others")

    async def test_a_token_without_the_scope(self):
        self.twitch.fail_with = (401, "Missing scope: channel:manage:redemptions")
        with self.assertRaisesRegex(RuntimeError, "--all-scopes"):
            await self.client.sync_rewards([spec(1)], seq=1)

    async def test_redemption_status(self):
        await self.client.set_redemption_status("r1", "red1", "CANCELED")
        self.assertEqual(self.twitch.redemptions, [({"broadcaster_id": "100", "reward_id": "r1", "id": "red1"}, "CANCELED")])
        with self.assertRaises(RuntimeError):
            await self.client.set_redemption_status("r1", "red1", "REFUND_PLEASE")


if __name__ == "__main__":
    unittest.main()
