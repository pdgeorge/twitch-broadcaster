(function () {
  const chatBox = document.getElementById('chat-box');

  function appendChatLine(html) {
    const line = document.createElement('div');
    line.className = 'chat-line';
    line.innerHTML = html;
    chatBox.appendChild(line);
  }

  function connect() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(`${protocol}//${window.location.host}/ws/overlay`);

    socket.addEventListener('message', (event) => {
      try {
        const payload = JSON.parse(event.data);
        if (payload.type === 'chat.message' && payload.message_html) {
          appendChatLine(payload.message_html);
        }
      } catch (err) {
        console.error('Failed to parse message', err);
      }
    });

    socket.addEventListener('close', () => {
      setTimeout(connect, 2000);
    });

    socket.addEventListener('error', () => {
      socket.close();
    });
  }

  connect();
})();
