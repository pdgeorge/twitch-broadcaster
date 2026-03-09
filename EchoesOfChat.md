# Executive Summary for EchoesOfChat
## Twitch chat does raids
- Twitch chatters, if they chatted at all, will join in the raid that will happen later.
- Chatters are strongers, the more times they engage there is a multiplier according to logging in.
    - So if you log in 100 times, you gain more experience than someone who has only logged in 1 times.
    - log in = logedin += 1
    - chat exp = 1 * logged_in w/ cooldown of 1 tick (10s)
    - lurk command = 100 & logged_in (but, locks you out of chatting experience for 200 ticks (400s))
- When the raid happens, chatters fight against a boss, being led by Dabi.
## Reward
- Whats the reward? Fucked if I know. Pretty lights and messages saying "You did good!"
- COSMETICS!!!! We can't have each chatter just being a little fkn... same blob
- (stream) MONEY!!!
    - Use money to buy cosmetics, 
    - GIVE money to other people
    - GAMBA money
    - USE money for announcements etc. (Other overlay game things)
## Example for DB
- Chatter_mcchatterino x-money, x-exp, x-cosmetics, x-logins

## MOSCOW
## MUST
- Chatters in to db, has money, exp, cosmetics (dict?), logins
- Read chatters from db and be able to increase exp, money, logins, modify values according to `events`
- gain exp from chatting
- Fix !lurk command
- Use values FROM the db to accomplish stuff
    - That stuff is...
        - We create a boss to kill (either dynamically or from the db) (outside of MVP, just use 1hp enemy named Jeff)
        - Collect the total exp from all people who have chatted in the last `variable x = 10` minutes and use them to attack Jeff.
        - Print out the total damage and Jeff's current HP as well as how many people were involved.
            - Send it as a Twitch message

## Should
- ### UI
    - The UI needs each chatter to have a 'Dabling'
        - Name on Dabling + cosmetics
    - Decision: Transparant background or solid background?
    - "Raid Leader Dabi" needs an image + appearance
    - Must be able to move from left of screen to 'boss' on the right of the screen to kill Jeff, converging.
    - After killing Jeff, he will have a 'die' animation and send the same message to Twitch chat from earlier.

## Could
- People can receive money for killing Jeff
- People can spend money on cosmetics
- People can equip cosmetics
- People can use money to make announcements on overlay using !announce
- People get money by using pdgeorHeartMint in the 5 minutes after someone follows or subscribes or raids but only once per person. 
- Timeout people? Why not.
----
# FIX THE DAMNED CHAT COMMANDS
- Make it that I can add chat commands to a db
- Any message starting with a "!" -> "Check in the DB if there is a command associated with it"
    - If yes, execute that command
    - Make sure that you can have {var} in command
        - Test /marker or other "/" commands (/raid {var})
    - Create new command using "$"? (Add to DB) (Commands anyone can use)
    - Creaete new command using "%" (Add to DB) (Commands only mods or streamer can use)
----
# Ideas I need to get to "Eventually"
## Easily the most femboy coded idea ever
- Make the petties redeem work on the new model
    - 2D PNG for MVP
    - Has to have a specific point that can be moved on a transparant webpage
    - It just goes up/down based on that point
- Coded in Rust.