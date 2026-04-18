Вариант A (★★, ~8-10 шагов) — port conflict

  Симптом: оператор: «nginx не запускается» (тот же goal!).

  Реальная причина: на :80 уже сидит чужой процесс, поэтому nginx -t ОК но bind() failed.

  Воспроизвести (~30 сек):
  # на VM:
  systemctl stop nginx
  # опционально: сначала вернуть nginx.conf в норму (убрать ;;)
  sed -i 's/user www-data;;/user www-data;/' /etc/nginx/nginx.conf
  # занять порт 80 другим процессом в фоне:
  nohup python3 -m http.server 80 > /tmp/squatter.log 2>&1 &
  disown
  systemctl restart nginx   # упадёт

  Цепочка которой надо пройти: failed nginx → journal видит bind() failed (98: Address already in use) → net_listen видит кого-то на
  :80 → process_list идентифицирует chamillionaire → finding с обоими task_id. Загвоздка — модель должна не зацикливаться на конфиге
  (nginx -t бы прошёл), а пойти в net.

  ---
  
Вариант B (★★★, ~12-18 шагов) — DNS upstream + 502, симптом не там

  Симптом: оператор: «сайт отдаёт 502 на /api/health».

  Реальная причина: в /etc/nginx/sites-enabled/*.conf есть upstream backend { server backend.internal:8080; }, имя backend.internal не
  резолвится (нет в /etc/hosts, нет в DNS). nginx стартует ОК, nginx -t проходит, но proxy_pass → 502.

  Воспроизвести (~2 минуты):
  # на VM:
  sed -i 's/user www-data;;/user www-data;/' /etc/nginx/nginx.conf 2>/dev/null
  pkill -f 'http.server 80' 2>/dev/null
  cat > /etc/nginx/sites-available/api <<'EOF'
  upstream backend {
      server backend.internal:8080;
  }
  server {
      listen 80 default_server;
      location /api/ {
          proxy_pass http://backend/;
      }
      location /health {
          return 200 "ok\n";
      }
  }
EOF
  ln -sf /etc/nginx/sites-available/api /etc/nginx/sites-enabled/api
  rm -f /etc/nginx/sites-enabled/default
  systemctl restart nginx
  curl http://127.0.0.1/api/health   # должно 502

  Цепочка: проверить статус nginx (active!) → curl /api/health (нет такого коллектора → но journal nginx покажет 502 + upstream error)
  → journal_tail nginx → dns_resolve backend.internal (наш pure-go коллектор) → провал → file_read /etc/hosts или /etc/resolv.conf →
  file_read /etc/nginx/sites-enabled/api → finding "DNS resolution failure for upstream backend.internal".

  Модель должна не путать «nginx запущен» с «nginx работает», и должна умело комбинировать dns_resolve (pure-Go) + file_read (несколько
   файлов) + journal.

  ---
Вариант C (★★★★, ~15-25 шагов) — service crash loop, OOM-killer, маскировка

  Симптом: оператор: «сервис postgres не работает стабильно, периодически перезапускается».

  Реальная причина: postgres имеет workload который выедает память; OOM-killer его прибивает; systemd рестартит; через минуту опять.
  Текущее system_info покажет НИЗКУЮ память (postgres только что убили, освободилось). В журнале видно «exited code=killed
  status=9/KILL» но не сразу понятно кем. Реальный след — в dmesg / /var/log/kern.log: Out of memory: Killed process NNNN (postgres).

  Воспроизвести (~3 минуты):
  # на VM:
  apt-get install -y postgresql 2>/dev/null
  # жирный workload:
  cat > /etc/systemd/system/memhog.service <<'EOF'
  [Unit]
  Description=memory hog
  After=postgresql.service
  [Service]
  ExecStart=/bin/sh -c 'python3 -c "x=bytearray(900*1024*1024); import time; time.sleep(99999)"'
  [Install]
  WantedBy=multi-user.target
  EOF
  # симулируем oom через ограничение postgresql cgroup
  mkdir -p /etc/systemd/system/postgresql.service.d
  cat > /etc/systemd/system/postgresql.service.d/oom.conf <<'EOF'
  [Service]
  MemoryMax=64M
  Restart=on-failure
  RestartSec=3s
  EOF
  systemctl daemon-reload
  systemctl enable --now postgresql
  # через минуту oom-killer его съест и пойдёт цикл

  Цепочка: systemd_units → postgres activating (auto-restart) или failed → journal_tail postgresql → видит MemoryMax exceeded/OOM →
  system_info мем сейчас низкая (загвоздка) → search_artifact по journal artifact на pattern Out of memory|Killed process → file_read
  /etc/systemd/system/postgresql.service.d/oom.conf → finding "Memory cgroup limit too low → OOM-kill loop".

  Это самый интересный кейс — задействует search_artifact, multi-hop, защиту от ложного следа («память сейчас же ОК!»), и заставляет
  модель посмотреть в drop-in override.