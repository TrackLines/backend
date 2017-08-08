package es.tracklin.Client;

import es.tracklin.Error.Unauthorized;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.stereotype.Controller;
import org.springframework.web.bind.annotation.*;

@Controller
@RequestMapping("/v1/client")
public class ClientController {
    private Unauthorized unauthorized;

    public ClientController() {
        unauthorized = new Unauthorized();
    }

    @RequestMapping(method = RequestMethod.POST)
    public @ResponseBody ResponseEntity login(@RequestBody LoginModel loginModel) {
        Client client = new Client();
        client.login(loginModel);

        if (loginModel.getUsername().equals("bobby")) {
            return new ResponseEntity<Client>(client, HttpStatus.OK);
        }

        return new ResponseEntity<Unauthorized>(unauthorized, HttpStatus.FORBIDDEN);
    }

    @RequestMapping(method = RequestMethod.GET)
    public @ResponseBody ResponseEntity getData() {
        return new ResponseEntity(HttpStatus.UNAUTHORIZED);
    }
}
