import { Socket } from "node:net";
import { createInterface } from "node:readline";
import { createRequire } from "node:module";

var require = createRequire(import.meta.url);

global.require = require;

%s

(function () {
  const rl = createInterface({ input: process.stdin });

  rl.on("line", function (line) {
    const socket = new Socket();
    const token = line.split(":")[0];

    socket.connect(line.split(":")[1], function () {
      socket.write(token + " " + process.version + "\n");

      global.emit = function (arg) {
        console.log(token + arg);
      };

      let input = Buffer.alloc(0);
      let output = Buffer.alloc(0);

      socket.on("data", async (data) => {
        input = Buffer.concat([input, data]);
        while (input.length > 0) {
          let idx = input.indexOf(10);
          if (idx == -1) {
            break;
          }

          const js = JSON.parse(input.slice(0, idx).toString("utf8"));
          input = input.slice(idx + 1);

          if (input.length == 0) {
            input = Buffer.alloc(0);
          }

          let ret;
          try {
            ret = "v" + (await import('data:text/javascript;charset=utf-8,' + encodeURIComponent(js))).default;
          } catch (e) {
            ret = "e" + e;
          }
          output = Buffer.concat([
            output,
            Buffer.from(JSON.stringify(ret) + "\n", "utf8"),
          ]);
        }
        if (output.length > 0) {
          socket.write(output);
          output = output.slice(0, 0);
        }
      });
    });
  });
})();
