# Client check

Plays complete games against a Tango server using the Unity game's own network
code, so a deploy is checked the way the app uses it:

- sign-in, profile, starting wallet, shop purchase/equip, developer top-up
- a public 1v1 table where a bot fills the empty seat, played to the end;
  the wallet must move by exactly the server's payout
- a private 4-player room: create, join by its 6-digit code, start, play out
- friends: find by ID, request, accept
- dropping the connection mid-game, reconnecting and finishing

`game/` holds copies of the Unity project's `Assets/Scripts/Online/*.cs` and
`Assets/Scripts/Core/Models.cs`. Keep them in step when those files change.

```
dotnet run --project client-check -- https tango-backend.sssss.co.in 9443 <server-key>
```

It needs a non-prod server (it uses the developer top-up).
